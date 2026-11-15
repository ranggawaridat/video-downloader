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

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Database tidak tersedia")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// me: info kuota perangkat untuk frontend.
func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	deviceID := device.FromContext(r.Context())
	day := time.Now().UTC().Format("2006-01-02")

	var used int
	err := h.pool.QueryRow(r.Context(),
		"SELECT downloads FROM usage_daily WHERE device_id = $1 AND day = $2",
		deviceID, day).Scan(&used)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		used = 0
	}
	remaining := h.cfg.FreeDailyLimit - used
	if remaining < 0 {
		remaining = 0
	}
	now := time.Now().UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)

	writeJSON(w, http.StatusOK, map[string]any{
		"used_today":  used,
		"daily_limit": h.cfg.FreeDailyLimit,
		"remaining":   remaining,
		"max_height":  h.cfg.MaxHeight,
		"resets_at":   next.Format(time.RFC3339),
	})
}

// metadata: analisis URL jadi judul/thumbnail/daftar resolusi.
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
		"max_height": h.cfg.MaxHeight,
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
	if req.Format != "audio" && parseHeight(req.Format) > h.cfg.MaxHeight {
		writeError(w, http.StatusBadRequest, "quality_not_allowed",
			"Kualitas "+req.Format+"p belum tersedia di paket gratis (maksimal "+strconv.Itoa(h.cfg.MaxHeight)+"p)")
		return
	}

	deviceID := device.FromContext(r.Context())

	// Satu unduhan aktif per URL per perangkat.
	var existing string
	err = h.pool.QueryRow(r.Context(),
		"SELECT id::text FROM downloads WHERE device_id = $1 AND url = $2 AND status IN ('queued','running')",
		deviceID, u.String()).Scan(&existing)
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
		INSERT INTO usage_daily (device_id, day, downloads)
		VALUES ($1, $2::date, 1)
		ON CONFLICT (device_id, day)
		DO UPDATE SET downloads = usage_daily.downloads + 1
		WHERE usage_daily.downloads < $3
		RETURNING downloads`,
		deviceID, day, h.cfg.FreeDailyLimit).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusTooManyRequests, "quota_exceeded",
			"Kuota unduhan gratis hari ini sudah habis. Coba lagi besok.")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	var id string
	err = h.pool.QueryRow(r.Context(),
		"INSERT INTO downloads (device_id, url, format) VALUES ($1, $2, $3) RETURNING id::text",
		deviceID, u.String(), req.Format).Scan(&id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "status": "queued"})
}

func (h *Handler) listDownloads(w http.ResponseWriter, r *http.Request) {
	deviceID := device.FromContext(r.Context())
	rows, err := h.pool.Query(r.Context(), selectDownloads+`
		WHERE device_id = $1 ORDER BY created_at DESC LIMIT 20`, deviceID)
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
	deviceID := device.FromContext(r.Context())
	d, err := scanDownload(h.pool.QueryRow(r.Context(), selectDownloads+
		" WHERE id = $1 AND device_id = $2", id, deviceID))
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
	deviceID := device.FromContext(r.Context())
	ct, err := h.pool.Exec(r.Context(), `
		UPDATE downloads SET status = 'canceled', finished_at = now()
		WHERE id = $1 AND device_id = $2 AND status IN ('queued','running')`,
		id, deviceID)
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

// serveFile mengirim file hanya ke pemilik unduhan (cek device_id).
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	deviceID := device.FromContext(r.Context())

	var owner, storagePath, filename, status string
	err := h.pool.QueryRow(r.Context(),
		"SELECT device_id, storage_path, filename, status FROM downloads WHERE id = $1", id,
	).Scan(&owner, &storagePath, &filename, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != deviceID) {
		writeError(w, http.StatusNotFound, "not_found", "File tidak ditemukan")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
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
