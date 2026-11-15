package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"videodl/backend/internal/auth"
	"videodl/backend/internal/billing"
	"videodl/backend/internal/device"
)

type userKey struct{}

func userFromContext(ctx context.Context) *auth.User {
	u, _ := ctx.Value(userKey{}).(*auth.User)
	return u
}

// sessionMiddleware memuat user dari cookie session (jika ada) ke context.
func (h *Handler) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(auth.SessionCookie); err == nil && c.Value != "" {
			u, err := auth.UserBySession(r.Context(), h.pool, c.Value)
			if err == nil && u != nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- subject: identitas kuota/plan saat ini ----------

// subject menggabungkan identitas (user atau device anonim) dengan plan-nya.
type subject struct {
	Key    string // "u:<user_id>" atau "d:<device_id>" untuk usage_daily
	UserID string // "" untuk anonim
	Email  string
	Plan   *billing.Plan
}

func (h *Handler) currentSubject(r *http.Request) (*subject, error) {
	if u := userFromContext(r.Context()); u != nil {
		pl, err := billing.ActivePlanForUser(r.Context(), h.pool, u.ID)
		if err != nil {
			return nil, err
		}
		return &subject{Key: "u:" + u.ID, UserID: u.ID, Email: u.Email, Plan: pl}, nil
	}
	pl, err := billing.FreePlan(r.Context(), h.pool)
	if err != nil {
		return nil, err
	}
	return &subject{
		Key:  "d:" + device.FromContext(r.Context()),
		Plan: pl,
	}, nil
}

// ---------- handler auth ----------

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailRe.MatchString(email) || len(email) > 254 {
		writeError(w, http.StatusBadRequest, "invalid_email", "Email tidak valid")
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 100 {
		writeError(w, http.StatusBadRequest, "weak_password", "Password minimal 8 karakter")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	var id string
	err = h.pool.QueryRow(r.Context(),
		"INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id::text, email",
		email, hash).Scan(&id, &email)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeError(w, http.StatusConflict, "email_taken", "Email sudah terdaftar")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	h.finishLogin(w, r, &auth.User{ID: id, Email: email}, http.StatusCreated)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	var id, hash string
	err := h.pool.QueryRow(r.Context(),
		"SELECT id::text, password_hash FROM users WHERE email = $1", email,
	).Scan(&id, &hash)
	ok := false
	if err == nil {
		ok, _ = auth.VerifyPassword(hash, req.Password)
	}
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", auth.ErrInvalidCredentials.Error())
		return
	}

	h.finishLogin(w, r, &auth.User{ID: id, Email: email}, http.StatusOK)
}

// finishLogin: buat session, pindahkan riwayat device anonim ke akun, set cookie.
func (h *Handler) finishLogin(w http.ResponseWriter, r *http.Request, u *auth.User, status int) {
	deviceID := device.FromContext(r.Context())

	// Claim: riwayat unduhan yang dibuat anonim di browser ini menjadi milik akun.
	if deviceID != "" {
		_, _ = h.pool.Exec(r.Context(),
			"UPDATE downloads SET user_id = $1::uuid WHERE device_id = $2 AND user_id IS NULL",
			u.ID, deviceID)
	}

	token, _, err := auth.CreateSession(r.Context(), h.pool, u.ID, auth.SessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	setSessionCookie(w, token, int(auth.SessionTTL/time.Second))
	writeJSON(w, status, map[string]any{"authenticated": true, "email": u.Email})
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		_ = auth.DestroySession(r.Context(), h.pool, c.Value)
	}
	setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}
