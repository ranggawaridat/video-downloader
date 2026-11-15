// Package jobs: pembuatan job unduhan terpusat — validasi URL/format,
// batas kualitas plan, cek duplikat, kuota harian atomis, dan insert ke
// tabel downloads (antrean kerja). Dipakai handler API; klien lain (mis.
// integrasi baru) wajib memakai paket ini, bukan menyalin logikanya.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"videodl/backend/internal/billing"
	"videodl/backend/internal/validate"
)

// Error adalah penolakan domain dengan kode yang dipakai API (dan
// frontend) — jangan diubah tanpa sinkronisasi dengan web/app.js.
type Error struct {
	Code    string // invalid_url|invalid_format|quality_not_allowed|duplicate|quota_exceeded
	Message string // pesan ramah pengguna (Indonesia)
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Identity pemesan job: salah satu saja yang diisi.
type Identity struct {
	UserID   string // uuid akun web ("" jika anonim)
	DeviceID string // id perangkat anonim dari cookie vd_device
}

// Subject kunci kuota usage_daily.
func (i Identity) Subject() string {
	if i.UserID != "" {
		return "u:" + i.UserID
	}
	return "d:" + i.DeviceID
}

func (i Identity) userIDParam() any {
	if i.UserID == "" {
		return nil
	}
	return i.UserID
}

// Create memvalidasi permintaan lalu memasukkan job ke antrean.
// Selain mengembalikan id job, error bisa bertipe *Error.
func Create(ctx context.Context, pool *pgxpool.Pool, id Identity, plan *billing.Plan, rawURL, format string) (string, error) {
	u, err := validate.URL(rawURL)
	if err != nil {
		return "", &Error{Code: "invalid_url", Message: err.Error()}
	}
	if !validFormat(format) {
		return "", &Error{Code: "invalid_format",
			Message: "Format harus 360, 480, 720, 1080, atau audio"}
	}
	if format != "audio" && parseHeight(format) > plan.MaxHeight {
		return "", &Error{Code: "quality_not_allowed",
			Message: fmt.Sprintf("Kualitas %sp belum tersedia di paket %s (maksimal %dp)",
				format, plan.Name, plan.MaxHeight)}
	}

	// Satu unduhan aktif per URL per identitas.
	var existing string
	err = pool.QueryRow(ctx, `
		SELECT id::text FROM downloads
		WHERE status IN ('queued','running')
		  AND url = $3
		  AND (($1::uuid IS NOT NULL AND user_id = $1::uuid)
		       OR (user_id IS NULL AND device_id = $2))`,
		id.userIDParam(), id.DeviceID, u.String()).Scan(&existing)
	if err == nil {
		return "", &Error{Code: "duplicate",
			Message: "Sudah ada proses unduhan untuk link ini"}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	// Kuota harian atomis: increment hanya jika masih di bawah batas.
	day := time.Now().UTC().Format("2006-01-02")
	var used int
	err = pool.QueryRow(ctx, `
		INSERT INTO usage_daily (subject, day, downloads)
		VALUES ($1, $2::date, 1)
		ON CONFLICT (subject, day)
		DO UPDATE SET downloads = usage_daily.downloads + 1
		WHERE usage_daily.downloads < $3
		RETURNING downloads`,
		id.Subject(), day, plan.DailyLimit).Scan(&used)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", &Error{Code: "quota_exceeded",
			Message: fmt.Sprintf("Kuota %s hari ini sudah habis. Coba lagi besok.", plan.Name)}
	}
	if err != nil {
		return "", err
	}

	// Retensi file mengikuti plan pada saat job dibuat.
	var jobID string
	err = pool.QueryRow(ctx, `
		INSERT INTO downloads (device_id, user_id, url, format, expires_at)
		VALUES ($1, $2::uuid, $3, $4, now() + make_interval(hours => $5))
		RETURNING id::text`,
		id.DeviceID, id.userIDParam(), u.String(), format, plan.RetentionHours).Scan(&jobID)
	if err != nil {
		return "", err
	}
	return jobID, nil
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
