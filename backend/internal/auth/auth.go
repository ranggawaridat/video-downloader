// Package auth: password hashing (argon2id) dan session berbasis cookie.
// Token session acak 32 byte; yang disimpan di DB hanya SHA-256-nya.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

const SessionCookie = "vd_session"

// SessionTTL masa berlaku cookie sesi.
const SessionTTL = 30 * 24 * time.Hour

// ErrInvalidCredentials dipakai agar login tidak membocorkan
// apakah email terdaftar atau tidak.
var ErrInvalidCredentials = errors.New("email atau password salah")

// User identitas minimal yang dibawa di context.
type User struct {
	ID    string
	Email string
	Role  string // user|admin
}

// ---------- password (argon2id, format PHC) ----------

const (
	argonMemory  uint32 = 64 * 1024
	argonTime    uint32 = 3
	argonThreads uint8  = 2
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// HashPassword menghasilkan string PHC: $argon2id$v=19$m=...,t=...,p=...$salt$hash
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword memverifikasi password terhadap hash PHC (constant-time).
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("format hash tidak dikenal")
	}

	var version uint32
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("versi argon2 tidak valid: %w", err)
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, fmt.Errorf("parameter argon2 tidak valid: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("salt tidak valid: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("hash tidak valid: %w", err)
	}

	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// ---------- session ----------

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession membuat token session baru dan menyimpan hash-nya.
func CreateSession(ctx context.Context, pool *pgxpool.Pool, userID string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token = hex.EncodeToString(raw)
	expiresAt = time.Now().Add(ttl).UTC()
	_, err = pool.Exec(ctx,
		"INSERT INTO sessions (user_id, token_hash, expires_at) VALUES ($1::uuid, $2, $3)",
		userID, hashToken(token), expiresAt)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// UserBySession mengembalikan user untuk token yang valid, atau nil.
func UserBySession(ctx context.Context, pool *pgxpool.Pool, token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	var u User
	err := pool.QueryRow(ctx, `
		SELECT u.id::text, u.email, u.role
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`,
		hashToken(token)).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// DestroySession menghapus session (logout).
func DestroySession(ctx context.Context, pool *pgxpool.Pool, token string) error {
	if token == "" {
		return nil
	}
	_, err := pool.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", hashToken(token))
	return err
}

// EnsureAdmin membuat atau mempromosikan akun admin dari env saat startup.
// Dipakai untuk bootstrap; kata sandi tidak diubah jika akun sudah ada.
func EnsureAdmin(ctx context.Context, pool *pgxpool.Pool, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || password == "" {
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO users (email, password_hash, role)
		VALUES ($1, $2, 'admin')
		ON CONFLICT (email) DO UPDATE SET role = 'admin'`, email, hash)
	return err
}
