package config

import (
	"os"
	"strconv"
	"time"
)

// Config memuat seluruh konfigurasi aplikasi dari environment variable.
type Config struct {
	Addr              string        // alamat listen API, mis. ":8080"
	DatabaseURL       string        // DSN PostgreSQL
	FilesDir          string        // direktori file hasil download
	StaticDir         string        // direktori frontend (web) yang disajikan API
	YtDlpPath         string        // path/nama binary yt-dlp
	FfmpegPath        string        // path/nama binary ffmpeg
	FreeDailyLimit    int           // kuota unduhan gratis per perangkat per hari
	MaxHeight         int           // tinggi video maksimum (paket gratis)
	MaxFilesizeMB     int           // batas ukuran file per unduhan (MB)
	FileTTL           time.Duration // lama file disimpan sebelum dihapus
	JobTimeout        time.Duration // batas waktu maksimum satu job unduhan
	MaxAttempts       int           // percobaan ulang maksimum per job
	WorkerConcurrency int           // jumlah goroutine worker yang memproses job paralel
}

func Load() Config {
	return Config{
		Addr:              getEnv("ADDR", ":8080"),
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://videodl:videodl@localhost:5432/videodl?sslmode=disable"),
		FilesDir:          getEnv("FILES_DIR", "./data/files"),
		StaticDir:         getEnv("STATIC_DIR", "../web"),
		YtDlpPath:         getEnv("YTDLP_PATH", "yt-dlp"),
		FfmpegPath:        getEnv("FFMPEG_PATH", "ffmpeg"),
		FreeDailyLimit:    getEnvInt("FREE_DAILY_LIMIT", 3),
		MaxHeight:         getEnvInt("MAX_HEIGHT", 720),
		MaxFilesizeMB:     getEnvInt("MAX_FILESIZE_MB", 500),
		FileTTL:           time.Duration(getEnvInt("FILE_TTL_HOURS", 6)) * time.Hour,
		JobTimeout:        time.Duration(getEnvInt("JOB_TIMEOUT_MINUTES", 20)) * time.Minute,
		MaxAttempts:       getEnvInt("MAX_ATTEMPTS", 3),
		WorkerConcurrency: getEnvInt("WORKER_CONCURRENCY", 2),
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
