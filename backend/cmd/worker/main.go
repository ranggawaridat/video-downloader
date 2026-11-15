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
	"videodl/backend/internal/worker"
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

	worker.New(pool, cfg, logger).Run(ctx)
	slog.Info("worker stopped")
}
