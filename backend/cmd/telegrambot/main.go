// cmd/telegrambot: proses bot Telegram. Menambah binary ketiga dari
// codebase yang sama — tidak menjalankan worker; hanya long polling
// Bot API + notifikasi hasil job.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"videodl/backend/internal/config"
	"videodl/backend/internal/db"
	"videodl/backend/internal/migrate"
	"videodl/backend/internal/telegram"
	"videodl/backend/internal/ytdlp"
)

func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := migrate.Run(ctx, pool); err != nil {
		slog.Error("migration failed", "err", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(cfg.FilesDir, 0o755); err != nil {
		slog.Error("create files dir failed", "dir", cfg.FilesDir, "err", err)
		os.Exit(1)
	}

	bot := telegram.New(pool, cfg.TelegramBotToken, ytdlp.Options{
		YtDlpPath:     cfg.YtDlpPath,
		FfmpegPath:    cfg.FfmpegPath,
		MaxFilesizeMB: cfg.MaxFilesizeMB,
	}, telegram.Config{
		PublicBaseURL: cfg.PublicBaseURL,
		FilesDir:      cfg.FilesDir,
	}, logger)

	if err := bot.Run(ctx); err != nil {
		slog.Error("bot stopped", "err", err)
	}
}
