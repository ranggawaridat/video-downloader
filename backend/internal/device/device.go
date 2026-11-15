// Package device mengelola identitas perangkat anonim via cookie.
// Untuk MVP tanpa akun, kuota dan riwayat unduhan terikat pada cookie ini.
// Nanti identitas email dapat dipetakan ke device_id yang sama.
package device

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
)

type ctxKey struct{}

const CookieName = "vd_device"

// Middleware memastikan setiap request memiliki device_id: dibaca dari cookie
// atau dibuat baru lalu dikirim via Set-Cookie.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ""
		if c, err := r.Cookie(CookieName); err == nil && validID(c.Value) {
			id = c.Value
		}
		if id == "" {
			id = newUUID()
			http.SetCookie(w, &http.Cookie{
				Name:     CookieName,
				Value:    id,
				Path:     "/",
				MaxAge:   60 * 60 * 24 * 365,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

// FromContext mengambil device_id dari request context.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func validID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand gagal hanya jika sistem bermasalah
	}
	b[6] = (b[6] & 0x0f) | 0x40 // versi 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
