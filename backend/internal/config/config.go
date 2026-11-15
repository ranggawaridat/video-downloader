package config

import (
	"os"
	"strconv"
	"time"
)

// Config memuat seluruh konfigurasi aplikasi dari environment variable.
type Config struct {
	Addr               string // alamat listen API, mis. ":8080"
	DatabaseURL        string // DSN PostgreSQL
	FilesDir           string // direktori file hasil download
	StaticDir          string // direktori frontend (web) yang disajikan API
	YtDlpPath          string // path/nama binary yt-dlp
	FfmpegPath         string // path/nama binary ffmpeg
	AdminEmail         string // akun admin dibuat/dipromosikan saat startup (kosong = lewati)
	AdminPassword      string
	BillingMode        string        // manual|midtrans|stripe|telegram (manual = aktivasi instan dev)
	MidtransServerKey  string        // server key Midtrans (kosong = belum dikonfigurasi)
	MidtransProduction bool          // true = app.midtrans.com, false = sandbox
	TelegramBotToken   string        // token @BotFather (kosong = bot off)
	PublicBaseURL      string        // URL publik untuk link file dari bot
	MaxFilesizeMB      int           // batas ukuran file per unduhan (MB)
	JobTimeout         time.Duration // batas waktu maksimum satu job unduhan
	MaxAttempts        int           // percobaan ulang maksimum per job
	WorkerConcurrency  int           // jumlah goroutine worker yang memproses job paralel
}

func Load() Config {
	return Config{
		Addr:               getEnv("ADDR", ":8080"),
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://videodl:videodl@localhost:5432/videodl?sslmode=disable"),
		FilesDir:           getEnv("FILES_DIR", "./data/files"),
		StaticDir:          getEnv("STATIC_DIR", "../web"),
		YtDlpPath:          getEnv("YTDLP_PATH", "yt-dlp"),
		FfmpegPath:         getEnv("FFMPEG_PATH", "ffmpeg"),
		AdminEmail:         getEnv("ADMIN_EMAIL", ""),
		AdminPassword:      getEnv("ADMIN_PASSWORD", ""),
		BillingMode:        getEnv("BILLING_MODE", "manual"),
		MidtransServerKey:  getEnv("MIDTRANS_SERVER_KEY", ""),
		MidtransProduction: getEnvBool("MIDTRANS_IS_PRODUCTION", false),
		TelegramBotToken:   getEnv("TELEGRAM_BOT_TOKEN", ""),
		PublicBaseURL:      getEnv("PUBLIC_BASE_URL", "http://localhost:8080"),
		MaxFilesizeMB:      getEnvInt("MAX_FILESIZE_MB", 500),
		JobTimeout:         time.Duration(getEnvInt("JOB_TIMEOUT_MINUTES", 20)) * time.Minute,
		MaxAttempts:        getEnvInt("MAX_ATTEMPTS", 3),
		WorkerConcurrency:  getEnvInt("WORKER_CONCURRENCY", 2),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
