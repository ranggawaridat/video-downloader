package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"videodl/backend/internal/device"
	"videodl/backend/internal/validate"
	"videodl/backend/internal/ytdlp"
)

// downloadDTO adalah representasi satu unduhan untuk klien.
type downloadDTO struct {
	ID           string     `json:"id"`
	URL          string     `json:"url"`
	Provider     string     `json:"provider"`
	Title        string     `json:"title"`
	Thumbnail    string     `json:"thumbnail"`
	DurationSecs int        `json:"duration_secs"`
	Format       string     `json:"format"`
	Status       string     `json:"status"`
	Progress     int        `json:"progress"`
	FileSize     int64      `json:"file_size"`
	Filename     string     `json:"filename"`
	StoragePath  string     `json:"-"`
	FileURL      string     `json:"file_url,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

const selectDownloads = `
	SELECT id::text, url, provider, title, thumbnail, duration_secs, format,
	       status, progress, file_size, filename, storage_path, error_code, error_message,
	       created_at, expires_at
	FROM downloads`

func scanDownload(row pgx.Row) (downloadDTO, error) {
	var d downloadDTO
	err := row.Scan(&d.ID, &d.URL, &d.Provider, &d.Title, &d.Thumbnail, &d.DurationSecs,
		&d.Format, &d.Status, &d.Progress, &d.FileSize, &d.Filename, &d.StoragePath,
		&d.ErrorCode, &d.ErrorMessage, &d.CreatedAt, &d.ExpiresAt)
	if err == nil && d.Status == "completed" && d.StoragePath != "" {
		d.FileURL = "/api/files/" + d.ID
	}
	return d, err
}

// ownershipClause: baris milik user login, atau (untuk anonim) milik device ini.
const ownershipClause = `
	($1::uuid IS NOT NULL AND user_id = $1::uuid)
	OR (user_id IS NULL AND device_id = $2)`

// userIDParam mengembalikan uuid user (string) atau nil untuk anonim.
func userIDParam(sub *subject) any {
	if sub.UserID == "" {
		return nil
	}
	return sub.UserID
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Database tidak tersedia")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// me: identitas, plan aktif, dan kuota hari ini.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	day := time.Now().UTC().Format("2006-01-02")

	var used int
	err = h.pool.QueryRow(r.Context(),
		"SELECT downloads FROM usage_daily WHERE subject = $1 AND day = $2",
		sub.Key, day).Scan(&used)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		used = 0
	}
	remaining := sub.Plan.DailyLimit - used
	if remaining < 0 {
		remaining = 0
	}
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)

	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": sub.UserID != "",
		"email":         sub.Email,
		"role":          roleOf(r),
		"plan": map[string]any{
			"code":            sub.Plan.Code,
			"name":            sub.Plan.Name,
			"daily_limit":     sub.Plan.DailyLimit,
			"max_height":      sub.Plan.MaxHeight,
			"retention_hours": sub.Plan.RetentionHours,
			"price_idr":       sub.Plan.PriceIDR,
		},
		"used_today": used,
		"remaining":  remaining,
		"resets_at":  next.Format(time.RFC3339),
	})
}

// roleOf: role user login ("" untuk anonim) tanpa risiko nil pointer.
func roleOf(r *http.Request) string {
	if u := userFromContext(r.Context()); u != nil {
		return u.Role
	}
	return ""
}

// metadata: analisis URL jadi judul/thumbnail/daftar resolusi.
// Batas resolusi sesuai plan pemohon.
func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	u, err := validate.URL(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	}

	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	mctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	m, err := ytdlp.Metadata(mctx, h.yt, u.String())
	if err != nil {
		h.ytError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":        u.String(),
		"provider":   strings.ToLower(m.Extractor),
		"title":      m.Title,
		"thumbnail":  m.Thumbnail,
		"duration":   int(m.Duration),
		"heights":    ytdlp.Heights(m),
		"max_height": sub.Plan.MaxHeight,
	})
}

// createDownload: validasi + kuota atomis + insert job.
func (h *Handler) createDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL    string `json:"url"`
		Format string `json:"format"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	u, err := validate.URL(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_url", err.Error())
		return
	}
	if !validFormat(req.Format) {
		writeError(w, http.StatusBadRequest, "invalid_format",
			"Format harus 360, 480, 720, 1080, atau audio")
		return
	}

	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	plan := sub.Plan
	if req.Format != "audio" && parseHeight(req.Format) > plan.MaxHeight {
		writeError(w, http.StatusBadRequest, "quality_not_allowed",
			"Kualitas "+req.Format+"p belum tersedia di paket "+plan.Name+
				" (maksimal "+strconv.Itoa(plan.MaxHeight)+"p)")
		return
	}

	// Satu unduhan aktif per URL per identitas.
	var existing string
	err = h.pool.QueryRow(r.Context(), `
		SELECT id::text FROM downloads
		WHERE status IN ('queued','running')
		  AND url = $3
		  AND `+ownershipClause,
		userIDParam(sub), device.FromContext(r.Context()), u.String()).Scan(&existing)
	if err == nil {
		writeError(w, http.StatusConflict, "duplicate", "Sudah ada proses unduhan untuk link ini")
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	// Kuota harian atomis: increment hanya jika masih di bawah batas.
	day := time.Now().UTC().Format("2006-01-02")
	var used int
	err = h.pool.QueryRow(r.Context(), `
		INSERT INTO usage_daily (subject, day, downloads)
		VALUES ($1, $2::date, 1)
		ON CONFLICT (subject, day)
		DO UPDATE SET downloads = usage_daily.downloads + 1
		WHERE usage_daily.downloads < $3
		RETURNING downloads`,
		sub.Key, day, plan.DailyLimit).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusTooManyRequests, "quota_exceeded",
			"Kuota "+plan.Name+" hari ini sudah habis. Coba lagi besok.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	// Retensi file mengikuti plan pada saat job dibuat.
	var id string
	err = h.pool.QueryRow(r.Context(), `
		INSERT INTO downloads (device_id, user_id, url, format, expires_at)
		VALUES ($1, $2::uuid, $3, $4, now() + make_interval(hours => $5))
		RETURNING id::text`,
		device.FromContext(r.Context()), userIDParam(sub), u.String(), req.Format,
		plan.RetentionHours).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "queued"})
}

func (h *Handler) listDownloads(w http.ResponseWriter, r *http.Request) {
	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	rows, err := h.pool.Query(r.Context(), selectDownloads+`
		WHERE `+ownershipClause+` ORDER BY created_at DESC LIMIT 20`,
		userIDParam(sub), device.FromContext(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer rows.Close()

	items := make([]downloadDTO, 0, 8)
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
			return
		}
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	d, err := scanDownload(h.pool.QueryRow(r.Context(), selectDownloads+`
		WHERE id = $3 AND `+ownershipClause,
		userIDParam(sub), device.FromContext(r.Context()), id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "Unduhan tidak ditemukan")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) cancelDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sub, err := h.currentSubject(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	ct, err := h.pool.Exec(r.Context(), `
		UPDATE downloads SET status = 'canceled', finished_at = now()
		WHERE id = $3 AND status IN ('queued','running') AND `+ownershipClause,
		userIDParam(sub), device.FromContext(r.Context()), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if ct.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "Unduhan tidak ditemukan atau sudah selesai")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "canceled"})
}

// serveFile mengirim file hanya ke pemilik unduhan.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var ownerUserID, rowDevice, storagePath, filename, status string
	err := h.pool.QueryRow(r.Context(), `
		SELECT COALESCE(user_id::text, ''), device_id, storage_path, filename, status
		FROM downloads WHERE id = $1`, id,
	).Scan(&ownerUserID, &rowDevice, &storagePath, &filename, &status)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	authUser := userFromContext(r.Context())
	isOwner := (ownerUserID != "" && authUser != nil && ownerUserID == authUser.ID) ||
		(ownerUserID == "" && rowDevice == device.FromContext(r.Context()))
	if errors.Is(err, pgx.ErrNoRows) || !isOwner {
		writeError(w, http.StatusNotFound, "not_found", "File tidak ditemukan")
		return
	}
	if status != "completed" || storagePath == "" {
		writeError(w, http.StatusGone, "expired", "File sudah tidak tersedia")
		return
	}

	// filepath.Base mencegah path traversal meski data DB berubah.
	path := filepath.Join(h.cfg.FilesDir, filepath.Base(storagePath))
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusGone, "expired", "File sudah dihapus dari server")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer f.Close()

	if filename == "" {
		filename = filepath.Base(path)
	}
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	http.ServeContent(w, r, filename, time.Time{}, f)
}

// ytError menerjemahkan error engine menjadi respons HTTP.
func (h *Handler) ytError(w http.ResponseWriter, err error) {
	var ye *ytdlp.Error
	if !errors.As(err, &ye) {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	status := http.StatusBadGateway
	switch ye.Code {
	case "invalid_url", "unsupported_url", "private_content", "too_large", "invalid_format":
		status = http.StatusBadRequest
	case "timeout":
		status = http.StatusGatewayTimeout
	case "engine_missing":
		status = http.StatusInternalServerError
	}
	writeError(w, status, ye.Code, ye.Message)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Body JSON tidak valid")
		return err
	}
	return nil
}

func validFormat(f string) bool {
	if f == "audio" {
		return true
	}
	h := parseHeight(f)
	return h > 0 && isStandardHeight(h)
}

func parseHeight(f string) int {
	n := 0
	for _, c := range f {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func isStandardHeight(h int) bool {
	switch h {
	case 144, 240, 360, 480, 720, 1080, 1440, 2160:
		return true
	}
	return false
}
