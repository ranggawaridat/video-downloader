package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// telegramLinkStart: buat kode sekali pakai (10 menit) untuk perintah
// /link <kode> di bot. Hanya untuk user yang sudah login.
func (h *Handler) telegramLinkStart(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu")
		return
	}

	// Bersihkan kode kedaluwarsa milik user ini.
	_, _ = h.pool.Exec(r.Context(),
		"DELETE FROM telegram_link_codes WHERE user_id = $1::uuid AND expires_at < now()", u.ID)

	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	code := hex.EncodeToString(b[:]) // 8 karakter hex

	_, err := h.pool.Exec(r.Context(), `
		INSERT INTO telegram_link_codes (code, user_id, expires_at)
		VALUES ($1, $2::uuid, now() + interval '10 minutes')`, code, u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code":       code,
		"expires_at": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})
}

// telegramUnlink: lepas ikatan chat Telegram dari akun ini (dari web).
func (h *Handler) telegramUnlink(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu")
		return
	}
	ct, err := h.pool.Exec(r.Context(),
		"DELETE FROM telegram_links WHERE user_id = $1::uuid", u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unlinked": ct.RowsAffected()})
}

// telegramStatus: apakah ada chat yang terikat ke akun ini (untuk UI web).
func (h *Handler) telegramStatus(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu")
		return
	}
	var chatID int64
	var linkedAt time.Time
	err := h.pool.QueryRow(r.Context(), `
		SELECT chat_id, created_at FROM telegram_links
		WHERE user_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		u.ID).Scan(&chatID, &linkedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"linked": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"linked":    true,
		"chat_id":   chatID,
		"linked_at": linkedAt,
	})
}
