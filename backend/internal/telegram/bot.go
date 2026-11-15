package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"videodl/backend/internal/billing"
	"videodl/backend/internal/jobs"
	"videodl/backend/internal/ytdlp"
)

// Config konfigurasi bot.
type Config struct {
	PublicBaseURL string // untuk link file, mis. http://localhost:8080
	FilesDir      string // lokasi file hasil unduhan
}

// Bot long-polling Bot API dan memproses command.
type Bot struct {
	pool     *pgxpool.Pool
	client   *Client
	yt       ytdlp.Options
	cfg      Config
	log      *slog.Logger
	filesDir string // FILES_DIR untuk membaca file yang dikirim

	// cache metadata untuk callback kualitas: id → data.
	metaMu  chan struct{} // semaphore (kapasitas 1) sebagai mutex
	meta    map[string]*metaEntry
	metaSeq int64
}

type metaEntry struct {
	ChatID  int64
	URL     string
	Title   string
	Heights []int
	Expire  time.Time
}

func New(pool *pgxpool.Pool, token string, yt ytdlp.Options, cfg Config, log *slog.Logger) *Bot {
	return &Bot{
		pool:     pool,
		client:   NewClient(token),
		yt:       yt,
		cfg:      cfg,
		log:      log,
		filesDir: cfg.FilesDir,
		metaMu:   make(chan struct{}, 1),
		meta:     map[string]*metaEntry{},
	}
}

// Run menjalankan bot sampai ctx selesai.
func (b *Bot) Run(ctx context.Context) error {
	if !b.client.Configured() {
		b.log.Warn("TELEGRAM_BOT_TOKEN kosong — bot idle menunggu token")
		<-ctx.Done() // idle, bukan exit: hindari restart loop compose
		return ctx.Err()
	}

	go b.notifyLoop(ctx)

	var offset int64
	for {
		updates, err := b.client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ae *APIError
			if errors.As(err, &ae) && ae.RetryAfter() > 0 {
				time.Sleep(ae.RetryAfter())
				continue
			}
			b.log.Error("getUpdates failed", "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.ID >= offset {
				offset = u.ID + 1
			}
			b.handle(ctx, u)
		}
	}
}

// ---------- identitas ----------

// identityFor: user_id (jika chat terikat akun web) atau device "tg:<chat_id>".
func (b *Bot) identityFor(ctx context.Context, chatID int64) (jobs.Identity, error) {
	var userID string
	err := b.pool.QueryRow(ctx,
		"SELECT COALESCE(user_id::text, '') FROM telegram_links WHERE chat_id = $1",
		chatID).Scan(&userID)
	if err != nil {
		if !isNoRows(err) {
			return jobs.Identity{}, err
		}
		userID = ""
	}
	if userID != "" {
		return jobs.Identity{UserID: userID}, nil
	}
	return jobs.Identity{DeviceID: "tg:" + strconv.FormatInt(chatID, 10)}, nil
}

func (b *Bot) planFor(ctx context.Context, id jobs.Identity) (*billing.Plan, error) {
	if id.UserID != "" {
		return billing.ActivePlanForUser(ctx, b.pool, id.UserID)
	}
	return billing.FreePlan(ctx, b.pool)
}

// ---------- update handling ----------

func (b *Bot) handle(ctx context.Context, u Update) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("bot panic", "err", r)
		}
	}()
	switch {
	case u.Message != nil:
		if u.Message.Chat.Type != "private" {
			return // MVP: hanya chat pribadi
		}
		b.handleMessage(ctx, u.Message)
	case u.Callback != nil && u.Callback.Message != nil:
		b.handleCallback(ctx, u.Callback)
	}
}

func (b *Bot) handleMessage(ctx context.Context, m *Msg) {
	text := strings.TrimSpace(m.Text)
	chatID := m.Chat.ID

	switch {
	case text == "/start" || text == "/help":
		b.send(ctx, chatID,
			"🎬 *VidGrab Bot*\nKirim link video (YouTube/TikTok/rl.) untuk diunduh.\n\n"+
				"Perintah:\n"+
				"/download <link> — unduh (pilih kualitas)\n"+
				"/status — status unduhan terakhir\n"+
				"/link <kode> — ikat ke akun web (kode dari situs)\n"+
				"/unlink — lepas ikatan akun\n\n"+
				"Tanpa ikatan: kuota gratis 3/hari, maks 720p.")
	case strings.HasPrefix(text, "/download"):
		url := strings.TrimSpace(strings.TrimPrefix(text, "/download"))
		if url == "" {
			b.send(ctx, chatID, "Contoh: /download https://youtu.be/xyz")
			return
		}
		b.analyzeAndOffer(ctx, chatID, url)
	case strings.HasPrefix(text, "/link"):
		code := strings.TrimSpace(strings.TrimPrefix(text, "/link"))
		b.handleLink(ctx, chatID, code)
	case strings.HasPrefix(text, "/unlink"):
		ct, err := b.pool.Exec(ctx,
			"DELETE FROM telegram_links WHERE chat_id = $1", chatID)
		if err != nil {
			b.send(ctx, chatID, "Gagal melepas ikatan. Coba lagi.")
			return
		}
		if ct.RowsAffected() == 0 {
			b.send(ctx, chatID, "Chat ini belum terikat akun.")
			return
		}
		b.send(ctx, chatID, "✅ Ikatan akun dilepas. Kembali ke kuota gratis.")
	case strings.HasPrefix(text, "/status"):
		b.handleStatus(ctx, chatID)
	case strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://"):
		b.analyzeAndOffer(ctx, chatID, text)
	default:
		b.send(ctx, chatID, "Kirim link video atau /help untuk daftar perintah.")
	}
}

// analyzeAndOffer: metadata yt-dlp → pilih kualitas via inline keyboard.
func (b *Bot) analyzeAndOffer(ctx context.Context, chatID int64, rawURL string) {
	identity, err := b.identityFor(ctx, chatID)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	plan, err := b.planFor(ctx, identity)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}

	m, err := ytdlp.Metadata(ctx, b.yt, rawURL)
	if err != nil {
		b.send(ctx, chatID, friendlyEngineError(err))
		return
	}

	// Resolusi tersedia, dibatasi plan, maks 5 pilihan.
	heights := []int{}
	for _, h := range ytdlp.Heights(m) {
		if h <= plan.MaxHeight {
			heights = append(heights, h)
		}
		if len(heights) == 5 {
			break
		}
	}

	key := b.newMetaKey()
	b.metaMu <- struct{}{}
	b.meta[key] = &metaEntry{
		ChatID: chatID, URL: rawURL, Title: m.Title,
		Heights: heights, Expire: time.Now().Add(15 * time.Minute),
	}
	<-b.metaMu

	var rows [][]InlineButton
	for _, h := range heights {
		rows = append(rows, []InlineButton{{
			Text: "🎥 " + strconv.Itoa(h) + "p",
			Data: "q:" + key + ":" + strconv.Itoa(h),
		}})
	}
	rows = append(rows, []InlineButton{{Text: "🎧 Audio MP3", Data: "q:" + key + ":audio"}})

	title := m.Title
	if len(title) > 60 {
		title = title[:57] + "…"
	}
	kb := &Keyboard{Rows: rows}
	b.send(ctx, chatID,
		"🎬 "+title+"\n\nPilih kualitas (plan "+plan.Name+", maks "+
			strconv.Itoa(plan.MaxHeight)+"p):", kb)
}

func (b *Bot) newMetaKey() string {
	b.metaSeq++
	// kunci unik + seq supaya beda antar percakapan.
	key := b.metaSeq + time.Now().UnixMilli()
	return strconv.FormatInt(key, 36)
}

func (b *Bot) handleCallback(ctx context.Context, cb *Cb) {
	_ = b.client.AnswerCallback(ctx, cb.ID, "")
	chatID := cb.Message.Chat.ID

	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 || parts[0] != "q" {
		return
	}
	b.metaMu <- struct{}{}
	entry, ok := b.meta[parts[1]]
	if ok && time.Now().After(entry.Expire) {
		delete(b.meta, parts[1])
		ok = false
	}
	<-b.metaMu
	if !ok || entry.ChatID != chatID {
		b.send(ctx, chatID, "Pilihan kedaluwarsa. Kirim ulang link-nya ya.")
		return
	}

	b.startJob(ctx, chatID, entry.URL, parts[2])
}

func (b *Bot) startJob(ctx context.Context, chatID int64, rawURL, format string) {
	identity, err := b.identityFor(ctx, chatID)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	plan, err := b.planFor(ctx, identity)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}

	id, err := jobs.Create(ctx, b.pool, identity, plan, rawURL, format)
	if err != nil {
		var je *jobs.Error
		if errors.As(err, &je) {
			b.send(ctx, chatID, "⚠️ "+je.Message)
			return
		}
		b.log.Error("create job failed", "err", err)
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	b.send(ctx, chatID,
		"⏳ Antri #"+id[:8]+"\nFile dikirim ke sini saat selesai (maks "+
			strconv.Itoa(plan.RetentionHours)+" jam retensi).")
}

func (b *Bot) handleLink(ctx context.Context, chatID int64, code string) {
	if code == "" {
		b.send(ctx, chatID,
			"Buka situs VidGrab → panel Telegram → ambil kode, lalu kirim:\n"+
				"/link <kode>\n\nKode berlaku 10 menit.")
		return
	}
	var userID string
	err := b.pool.QueryRow(ctx, `
		SELECT user_id::text FROM telegram_link_codes
		WHERE code = $1 AND expires_at > now()`, strings.ToUpper(strings.TrimSpace(code))).
		Scan(&userID)
	if isNoRows(err) {
		b.send(ctx, chatID, "❌ Kode tidak valid atau kedaluwarsa. Buat kode baru di situs.")
		return
	}
	if err != nil {
		b.log.Error("link lookup failed", "err", err)
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}

	// Konsumsi kode (sekali pakai) + ikat chat.
	_, err = b.pool.Exec(ctx,
		"DELETE FROM telegram_link_codes WHERE code = $1",
		strings.ToUpper(strings.TrimSpace(code)))
	if err != nil {
		b.log.Error("link consume failed", "err", err)
	}
	_, err = b.pool.Exec(ctx, `
		INSERT INTO telegram_links (chat_id, user_id)
		VALUES ($1, $2::uuid)
		ON CONFLICT (chat_id)
		DO UPDATE SET user_id = $2::uuid, updated_at = now()`, chatID, userID)
	if err != nil {
		b.log.Error("link save failed", "err", err)
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	b.send(ctx, chatID, "✅ Chat ini terikat ke akun VidGrab-mu. Kuota & plan akun dipakai.")
}

func (b *Bot) handleStatus(ctx context.Context, chatID int64) {
	identity, err := b.identityFor(ctx, chatID)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, status, COALESCE(NULLIF(title, ''), url), progress, error_code
		FROM downloads
		WHERE (($1::uuid IS NOT NULL AND user_id = $1::uuid)
		       OR (user_id IS NULL AND device_id = $2))
		ORDER BY created_at DESC LIMIT 5`,
		identity.UserID, identity.DeviceID)
	if err != nil {
		b.send(ctx, chatID, "Terjadi kesalahan, coba lagi.")
		return
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var id, status, title, errCode string
		var progress int
		if err := rows.Scan(&id, &status, &title, &progress, &errCode); err != nil {
			continue
		}
		if len(title) > 50 {
			title = title[:47] + "…"
		}
		switch status {
		case "completed":
			sb.WriteString("✅ " + title + "\n")
		case "failed":
			sb.WriteString("❌ " + title + " (" + errCode + ")\n")
		case "running":
			sb.WriteString(fmt.Sprintf("⏳ %s — %d%%\n", title, progress))
		case "queued":
			sb.WriteString("🕐 " + title + " (menunggu)\n")
		default:
			sb.WriteString("• " + title + " (" + status + ")\n")
		}
	}
	if sb.Len() == 0 {
		b.send(ctx, chatID, "Belum ada unduhan. Kirim link video untuk mulai.")
		return
	}
	b.send(ctx, chatID, "5 unduhan terakhir:\n"+strings.TrimRight(sb.String(), "\n"))
}

// ---------- notifier ----------

// notifyLoop mengirim file/hasil ke chat Telegram untuk job yang dibuat
// lewat bot (device_id 'tg:<chat_id>') dan belum diberitahukan.
func (b *Bot) notifyLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.notifyOnce(ctx)
		}
	}
}

type doneJob struct {
	ID       string
	Status   string
	Title    string
	ErrCode  string
	FileName string
	Path     string
	Size     int64
	ChatID   int64
}

func (b *Bot) notifyOnce(ctx context.Context) {
	rows, err := b.pool.Query(ctx, `
		SELECT d.id::text, d.status, COALESCE(NULLIF(d.title, ''), d.url),
		       d.error_code, d.filename,
		       CASE WHEN d.status = 'completed' THEN d.storage_path ELSE '' END,
		       d.file_size,
		       (regexp_replace(d.device_id, '^tg:', ''))::bigint
		FROM downloads d
		WHERE d.device_id LIKE 'tg:%'
		  AND d.notified_at IS NULL
		  AND d.status IN ('completed','failed','canceled')
		LIMIT 20`)
	if err != nil {
		b.log.Error("notify query failed", "err", err)
		return
	}
	defer rows.Close()

	var done []doneJob
	for rows.Next() {
		var j doneJob
		if err := rows.Scan(&j.ID, &j.Status, &j.Title, &j.ErrCode,
			&j.FileName, &j.Path, &j.Size, &j.ChatID); err == nil {
			done = append(done, j)
		}
	}
	rows.Close()

	for _, j := range done {
		b.deliverNotification(ctx, j)
	}
}

const maxSendDocMB = 45 // batas aman di bawah 50MB Bot API

func (b *Bot) deliverNotification(ctx context.Context, j doneJob) {
	title := j.Title
	if len(title) > 60 {
		title = title[:57] + "…"
	}

	switch j.Status {
	case "completed":
		path := filepath.Join(b.filesDir, filepath.Base(j.Path))
		f, err := os.Open(path)
		if err == nil {
			f.Close()
			if j.Size <= maxSendDocMB*1024*1024 {
				if err := b.client.SendDocument(ctx, j.ChatID, path, j.FileName); err == nil {
					b.markNotified(ctx, j.ID)
					return
				}
				b.log.Error("sendDocument failed", "id", j.ID, "err", err)
			}
		}
		// File > 45MB atau gagal upload → kirim link dengan kunci share.
		link := b.shareLink(ctx, j.ID)
		if link != "" {
			b.send(ctx, j.ChatID, "✅ "+title+"\n⬇️ "+link+
				"\n\n(valid "+fmt.Sprintf("%d", 24)+" jam; file >45MB diunduh via browser)")
			b.markNotified(ctx, j.ID)
			return
		}
		b.send(ctx, j.ChatID, "✅ "+title+"\nFile sudah selesai tapi tidak tersedia lagi.")
		b.markNotified(ctx, j.ID)
	case "failed":
		b.send(ctx, j.ChatID, "❌ Gagal: "+title+"\nAlasan: "+friendlyCode(j.ErrCode)+
			"\n\nKirim ulang link untuk mencoba lagi.")
		b.markNotified(ctx, j.ID)
	case "canceled":
		// dibatalkan dari web — cukup tandai tanpa notifikasi.
		b.markNotified(ctx, j.ID)
	}
}

// shareLink membuat file_key baru dan mengembalikan URL publik.
func (b *Bot) shareLink(ctx context.Context, id string) string {
	var kb [16]byte
	if _, err := rand.Read(kb[:]); err != nil {
		return ""
	}
	key := hex.EncodeToString(kb[:])
	if _, err := b.pool.Exec(ctx,
		"UPDATE downloads SET file_key = $1 WHERE id = $2::uuid", key, id); err != nil {
		b.log.Error("set file_key failed", "err", err)
		return ""
	}
	return b.cfg.PublicBaseURL + "/api/files/" + id + "?k=" + key
}

func (b *Bot) markNotified(ctx context.Context, id string) {
	_, _ = b.pool.Exec(ctx,
		"UPDATE downloads SET notified_at = now() WHERE id = $1::uuid", id)
}

func (b *Bot) send(ctx context.Context, chatID int64, text string, kb ...*Keyboard) {
	var k *Keyboard
	if len(kb) > 0 {
		k = kb[0]
	}
	if err := b.client.SendMessage(ctx, chatID, text, k); err != nil {
		b.log.Error("sendMessage failed", "chat_id", chatID, "err", err)
	}
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func friendlyCode(code string) string {
	switch code {
	case "private_content":
		return "konten privat/perlu login"
	case "unsupported_url":
		return "situs belum didukung"
	case "too_large":
		return "file melebihi batas ukuran"
	case "timeout":
		return "waktu unduhan habis"
	case "":
		return "tidak diketahui"
	default:
		return code
	}
}

func friendlyEngineError(err error) string {
	var ye *ytdlp.Error
	if errors.As(err, &ye) {
		return "⚠️ " + ye.Message
	}
	return "⚠️ Gagal menganalisis link, coba lagi."
}
