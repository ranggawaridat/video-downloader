// Package httpapi: routing, middleware, dan helper respons JSON.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"videodl/backend/internal/config"
	"videodl/backend/internal/device"
	"videodl/backend/internal/midtrans"
	"videodl/backend/internal/ytdlp"
)

type Handler struct {
	pool     *pgxpool.Pool
	cfg      config.Config
	yt       ytdlp.Options
	midtrans *midtrans.Client
	log      *slog.Logger
}

// New membangun router lengkap API + static file frontend.
func New(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) http.Handler {
	return NewHandler(pool, cfg, log).Routes()
}

// NewHandler membangun Handler. Diekspor supaya cmd/api dapat menjalankan
// background job (reconciler) yang berbagi pool & client Midtrans.
func NewHandler(pool *pgxpool.Pool, cfg config.Config, log *slog.Logger) *Handler {
	return &Handler{
		pool: pool,
		cfg:  cfg,
		yt: ytdlp.Options{
			YtDlpPath:     cfg.YtDlpPath,
			FfmpegPath:    cfg.FfmpegPath,
			MaxFilesizeMB: cfg.MaxFilesizeMB,
		},
		midtrans: midtrans.New(cfg.MidtransServerKey, cfg.MidtransProduction),
		log:      log,
	}
}

// Routes memasang seluruh endpoint API + static handler.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer, h.logMiddleware, device.Middleware, h.sessionMiddleware)

	r.Get("/api/health", h.health)
	r.Get("/api/me", h.me)
	r.Get("/api/plans", h.listPlans)
	r.Post("/api/auth/register", h.register)
	r.Post("/api/auth/login", h.login)
	r.Post("/api/auth/logout", h.logout)
	r.Post("/api/billing/checkout", h.checkout)
	r.Post("/api/billing/cancel", h.cancelSubscription)
	r.Get("/api/billing/payments/{id}", h.paymentStatus)
	r.Post("/api/billing/webhook/midtrans", h.midtransWebhook)
	r.Post("/api/metadata", h.metadata)
	r.Post("/api/downloads", h.createDownload)
	r.Get("/api/downloads", h.listDownloads)
	r.Get("/api/downloads/{id}", h.getDownload)
	r.Delete("/api/downloads/{id}", h.cancelDownload)
	r.Get("/api/files/{id}", h.serveFile)
	r.Post("/api/auth/telegram/link", h.telegramLinkStart)
	r.Delete("/api/auth/telegram/link", h.telegramUnlink)
	r.Get("/api/auth/telegram/status", h.telegramStatus)

	r.Route("/api/admin", func(r chi.Router) {
		r.Use(h.requireAdmin)
		r.Get("/stats/overview", h.adminOverview)
		r.Get("/users", h.adminListUsers)
		r.Post("/users/{id}/grant", h.adminGrantPlan)
		r.Get("/downloads", h.adminListDownloads)
		r.Post("/downloads/{id}/retry", h.adminRetryDownload)
		r.Patch("/plans/{code}", h.adminUpdatePlan)
	})

	r.Handle("/*", h.staticHandler())
	return r
}

// logMiddleware mencatat setiap request secara terstruktur.
func (h *Handler) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"ms", time.Since(start).Milliseconds(),
		)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
