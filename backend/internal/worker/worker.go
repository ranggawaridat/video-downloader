// Package worker mengambil job unduhan dari PostgreSQL (FOR UPDATE SKIP LOCKED),
// menjalankannya lewat yt-dlp, dan memperbarui status/progres.
//
// Ini job queue minimal berbasis DB. Nanti bisa diganti River/asynq
// tanpa mengubah skema downloads karena tabel inilah source of truth.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"videodl/backend/internal/config"
	"videodl/backend/internal/ytdlp"
)

const (
	claimInterval  = 2 * time.Second  // jeda antar-poll saat tidak ada job
	maintainEvery  = 60 * time.Second // interval maintenance
	stuckThreshold = 45 * time.Minute // job 'running' lebih lama dari ini dianggap macet
	orphanMinAge   = 2 * time.Hour    // file/dir yatim dihapus jika lebih tua dari ini
)

type Worker struct {
	pool *pgxpool.Pool
	cfg  config.Config
	log  *slog.Logger
	opts ytdlp.Options
}

func New(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) *Worker {
	return &Worker{
		pool: pool,
		cfg:  cfg,
		log:  log,
		opts: ytdlp.Options{
			YtDlpPath:     cfg.YtDlpPath,
			FfmpegPath:    cfg.FfmpegPath,
			MaxFilesizeMB: cfg.MaxFilesizeMB,
		},
	}
}

type job struct {
	ID     string
	URL    string
	Format string
}

// Run menjalankan N goroutine pemroses job plus loop maintenance sampai ctx dibatalkan.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started",
		"concurrency", w.cfg.WorkerConcurrency,
		"files_dir", w.cfg.FilesDir,
		"max_attempts", w.cfg.MaxAttempts)

	for i := 0; i < w.cfg.WorkerConcurrency; i++ {
		go w.loop(ctx, i)
	}

	t := time.NewTicker(maintainEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping")
			return
		case <-t.C:
			w.maintain(ctx)
		}
	}
}

func (w *Worker) loop(ctx context.Context, n int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		j, err := w.claim(ctx)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				w.log.Error("claim job failed", "worker", n, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(claimInterval):
			}
			continue
		}

		w.log.Info("job started", "id", j.ID, "url", j.URL, "format", j.Format, "worker", n)
		w.process(ctx, j)
	}
}

// claim mengunci satu job 'queued' atomis; SKIP LOCKED membuat beberapa worker
// tidak pernah mengambil job yang sama.
func (w *Worker) claim(ctx context.Context) (job, error) {
	var j job
	err := w.pool.QueryRow(ctx, `
		UPDATE downloads SET status = 'running', started_at = now(), attempts = attempts + 1
		WHERE id = (
			SELECT id FROM downloads
			WHERE status = 'queued'
			  AND attempts < $1
			  AND (next_run_at IS NULL OR next_run_at <= now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id::text, url, format`, w.cfg.MaxAttempts,
	).Scan(&j.ID, &j.URL, &j.Format)
	return j, err
}

// process menjalankan satu job: metadata -> unduh -> pindah ke FILES_DIR -> completed.
func (w *Worker) process(ctx context.Context, j job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Pengamat pembatalan: jika status di DB berubah dari 'running'
	// (mis. user menekan "Batalkan"), batalkan eksekusi yt-dlp.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-t.C:
				var status string
				if err := w.pool.QueryRow(jobCtx,
					"SELECT status FROM downloads WHERE id = $1", j.ID).Scan(&status); err != nil {
					continue
				}
				if status != "running" {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-watchDone
	}()

	workDir := filepath.Join(w.cfg.FilesDir, ".work", j.ID)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		w.handleFailure(j, &ytdlp.Error{Code: "storage_error", Message: err.Error(), Retryable: true})
		return
	}
	defer os.RemoveAll(workDir)

	// Progres: yt-dlp sering mengirim update, tulis ke DB maksimal ~1x/detik.
	var lastProgress time.Time
	onProgress := func(percent int, totalBytes int64) {
		if time.Since(lastProgress) < 900*time.Millisecond {
			return
		}
		lastProgress = time.Now()
		_, _ = w.pool.Exec(context.WithoutCancel(jobCtx),
			"UPDATE downloads SET progress = $1 WHERE id = $2 AND status = 'running'",
			percent, j.ID)
	}

	// 1. Metadata: untuk judul/thumbnail/provider dan validasi awal.
	media, err := ytdlp.Metadata(jobCtx, w.opts, j.URL)
	if err != nil {
		if errors.Is(err, ytdlp.ErrCanceled) {
			return
		}
		w.handleFailure(j, err)
		return
	}
	_, _ = w.pool.Exec(context.WithoutCancel(jobCtx),
		"UPDATE downloads SET title = $1, thumbnail = $2, duration_secs = $3, provider = $4 WHERE id = $5",
		media.Title, media.Thumbnail, int(media.Duration), strings.ToLower(media.Extractor), j.ID)

	// 2. Unduh.
	srcPath, size, err := ytdlp.Download(jobCtx, w.opts, workDir, j.URL, j.Format, onProgress)
	if err != nil {
		if errors.Is(err, ytdlp.ErrCanceled) {
			return
		}
		w.handleFailure(j, err)
		return
	}

	// 3. Pindahkan ke lokasi final.
	ext := strings.ToLower(filepath.Ext(srcPath))
	if ext == "" {
		ext = ".mp4"
	}
	finalPath := filepath.Join(w.cfg.FilesDir, j.ID+ext)
	if err := os.Rename(srcPath, finalPath); err != nil {
		w.handleFailure(j, &ytdlp.Error{Code: "storage_error", Message: err.Error(), Retryable: true})
		return
	}

	name := SafeName(media.Title) + ext
	_, err = w.pool.Exec(context.WithoutCancel(jobCtx), `
		UPDATE downloads
		SET status = 'completed', progress = 100, storage_path = $1, filename = $2,
		    file_size = $3, finished_at = now(),
		    expires_at = now() + make_interval(hours => $4)
		WHERE id = $5 AND status = 'running'`,
		j.ID+ext, name, size, int(w.cfg.FileTTL.Hours()), j.ID)
	if err != nil {
		w.log.Error("mark completed failed", "id", j.ID, "err", err)
		return
	}
	w.log.Info("job completed", "id", j.ID, "size", size, "file", name)
}

// handleFailure mengklasifikasi error lalu memutuskan: retry (queued + backoff)
// atau final gagal.
func (w *Worker) handleFailure(j job, err error) {
	var ye *ytdlp.Error
	code, msg, retryable := "provider_error", err.Error(), false
	if errors.As(err, &ye) {
		code, msg, retryable = ye.Code, ye.Message, ye.Retryable
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, msg, retryable = "timeout", "Waktu unduhan habis", true
	}

	// Status bisa saja sudah 'canceled' oleh pengguna — jangan timpa.
	var status string
	var attempts int
	qErr := w.pool.QueryRow(context.Background(),
		"SELECT status, attempts FROM downloads WHERE id = $1", j.ID).Scan(&status, &attempts)
	if qErr != nil {
		w.log.Error("read job status failed", "id", j.ID, "err", qErr)
		return
	}
	if status != "running" {
		return
	}

	if retryable && attempts < w.cfg.MaxAttempts {
		backoff := 30 * attempts // detik
		_, uErr := w.pool.Exec(context.Background(), `
			UPDATE downloads
			SET status = 'queued', progress = 0,
			    next_run_at = now() + make_interval(secs => $2),
			    error_code = $3, error_message = $4
			WHERE id = $1 AND status = 'running'`,
			j.ID, float64(backoff), code, msg)
		if uErr != nil {
			w.log.Error("requeue failed", "id", j.ID, "err", uErr)
		} else {
			w.log.Warn("job requeued", "id", j.ID, "attempt", attempts, "code", code)
		}
		return
	}

	_, uErr := w.pool.Exec(context.Background(), `
		UPDATE downloads
		SET status = 'failed', error_code = $2, error_message = $3, finished_at = now()
		WHERE id = $1 AND status = 'running'`,
		j.ID, code, msg)
	if uErr != nil {
		w.log.Error("mark failed", "id", j.ID, "err", uErr)
	} else {
		w.log.Warn("job failed", "id", j.ID, "code", code, "message", msg)
	}
}

// maintain: pulihkan job macet, hapus file kedaluwarsa, bersihkan artefak yatim.
func (w *Worker) maintain(ctx context.Context) {
	// 1. Job 'running' yang macet -> requeue atau final gagal.
	if _, err := w.pool.Exec(ctx, `
		UPDATE downloads
		SET status = 'queued', progress = 0, next_run_at = now() + make_interval(secs => 30)
		WHERE status = 'running'
		  AND started_at < now() - make_interval(mins => 45)
		  AND attempts < $1`, w.cfg.MaxAttempts); err != nil {
		w.log.Error("requeue stuck failed", "err", err)
	}
	if _, err := w.pool.Exec(ctx, `
		UPDATE downloads
		SET status = 'failed', error_code = 'timeout', error_message = 'Job macet, dibatalkan', finished_at = now()
		WHERE status = 'running'
		  AND started_at < now() - make_interval(mins => 45)
		  AND attempts >= $1`, w.cfg.MaxAttempts); err != nil {
		w.log.Error("fail stuck failed", "err", err)
	}

	// 2. Hapus file kedaluwarsa.
	rows, err := w.pool.Query(ctx, `
		SELECT id::text, storage_path FROM downloads
		WHERE status = 'completed' AND expires_at IS NOT NULL AND expires_at < now()`)
	if err != nil {
		w.log.Error("query expired failed", "err", err)
		return
	}
	type expired struct {
		id   string
		path string
	}
	var expiredList []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.path); err == nil {
			expiredList = append(expiredList, e)
		}
	}
	rows.Close()
	for _, e := range expiredList {
		if e.path != "" {
			_ = os.Remove(filepath.Join(w.cfg.FilesDir, filepath.Base(e.path)))
		}
		if _, err := w.pool.Exec(ctx, `
			UPDATE downloads SET status = 'expired', storage_path = '' WHERE id::text = $1`, e.id); err != nil {
			w.log.Error("mark expired failed", "id", e.id, "err", err)
		} else {
			w.log.Info("file expired", "id", e.id)
		}
	}

	// 3. Bersihkan file yatim (row hilang/crash) dan direktori .work basi.
	w.cleanupOrphans(ctx)
}

func (w *Worker) cleanupOrphans(ctx context.Context) {
	known := map[string]bool{}
	rows, err := w.pool.Query(ctx, "SELECT storage_path FROM downloads WHERE storage_path <> ''")
	if err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				known[filepath.Base(p)] = true
			}
		}
		rows.Close()
	}

	entries, err := os.ReadDir(w.cfg.FilesDir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if known[name] {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < orphanMinAge {
			continue
		}
		_ = os.Remove(filepath.Join(w.cfg.FilesDir, name))
		w.log.Info("orphan file removed", "file", name)
	}

	workRoot := filepath.Join(w.cfg.FilesDir, ".work")
	entries, err = os.ReadDir(workRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < orphanMinAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(workRoot, e.Name()))
		w.log.Info("stale workdir removed", "dir", e.Name())
	}
}

// SafeName membersihkan judul untuk dijadikan nama file yang aman.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r),
			r == ' ', r == '-', r == '_', r == '.':
			return r
		default:
			return -1
		}
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > 80 {
		r = r[:80]
	}
	s = strings.Trim(string(r), ". ")
	if s == "" {
		s = "video"
	}
	return s
}
