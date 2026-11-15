// Package ytdlp membungkus binary yt-dlp: metadata, daftar resolusi,
// dan eksekusi unduhan dengan pelaporan progres.
//
// Penting: kami tidak pernah meneruskan input pengguna ke shell —
// semua argumen dikirim langsung ke exec.Command.
package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrCanceled berarti job dibatalkan (status di DB bukan 'running' saat proses berjalan).
var ErrCanceled = errors.New("download canceled")

// Options konfigurasi engine.
type Options struct {
	YtDlpPath     string
	FfmpegPath    string
	MaxFilesizeMB int
}

// Media adalah subset JSON output `yt-dlp -J` yang kami perlukan.
type Media struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Duration  float64  `json:"duration"`
	Thumbnail string   `json:"thumbnail"`
	Extractor string   `json:"extractor_key"`
	Formats   []Format `json:"formats"`
}

type Format struct {
	Height         int    `json:"height"`
	VCodec         string `json:"vcodec"`
	ACodec         string `json:"acodec"`
	Ext            string `json:"ext"`
	Filesize       int64  `json:"filesize"`
	FilesizeApprox int64  `json:"filesize_approx"`
}

// Error adalah error terklasifikasi dari yt-dlp agar API bisa
// mengembalikan kode error yang bermakna.
type Error struct {
	Code      string // invalid_url|unsupported_url|private_content|too_large|timeout|provider_error|engine_missing
	Message   string // pesan ramah pengguna (Indonesia)
	Retryable bool   // layak dicoba ulang oleh worker
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func classify(stderr string) *Error {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "unsupported url"):
		return &Error{Code: "unsupported_url", Message: "Situs ini belum didukung"}
	case strings.Contains(s, "private video"), strings.Contains(s, "sign in to confirm"),
		strings.Contains(s, "this video is unavailable"), strings.Contains(s, "login required"),
		strings.Contains(s, "confirm your age"):
		return &Error{Code: "private_content", Message: "Konten tidak dapat diakses (privat/perlu login)"}
	case strings.Contains(s, "is not a valid url"):
		return &Error{Code: "invalid_url", Message: "URL tidak valid"}
	case strings.Contains(s, "larger than max-filesize"):
		return &Error{Code: "too_large", Message: "Ukuran file melebihi batas maksimum"}
	case strings.Contains(s, "unable to download"), strings.Contains(s, "http error"),
		strings.Contains(s, "connection"), strings.Contains(s, "timed out"):
		return &Error{Code: "provider_error", Message: "Gagal mengambil data dari penyedia", Retryable: true}
	default:
		snippet := stderr
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return &Error{Code: "provider_error", Message: strings.TrimSpace(snippet), Retryable: true}
	}
}

// Metadata mengambil informasi media via `yt-dlp -J`.
func Metadata(ctx context.Context, opts Options, rawURL string) (*Media, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{"-J", "--no-playlist", "--no-warnings", "--socket-timeout", "15", rawURL}
	stdout, stderr, err := run(cctx, opts.YtDlpPath, args)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, &Error{Code: "engine_missing", Message: "yt-dlp tidak ditemukan di server"}
		}
		if cctx.Err() == context.DeadlineExceeded {
			return nil, &Error{Code: "timeout", Message: "Waktu analisis habis", Retryable: true}
		}
		return nil, classify(string(stderr))
	}

	var m Media
	if err := json.Unmarshal(stdout, &m); err != nil {
		return nil, &Error{Code: "provider_error", Message: "Gagal membaca metadata", Retryable: true}
	}
	if m.Title == "" {
		m.Title = rawURL
	}
	return &m, nil
}

// Heights mengembalikan daftar resolusi unik yang tersedia, terbesar dulu.
func Heights(m *Media) []int {
	set := map[int]bool{}
	for _, f := range m.Formats {
		if f.Height > 0 {
			set[f.Height] = true
		}
	}
	out := make([]int, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

// Download menjalankan yt-dlp untuk mengunduh ke workDir.
// format: "audio" (MP3) atau string tinggi seperti "720".
// onProgress dipanggil setiap ada update progres (sudah dibatasi pemanggil).
// Mengembalikan path file jadi di workDir dan ukurannya dalam byte.
func Download(ctx context.Context, opts Options, workDir, rawURL, format string, onProgress func(percent int, totalBytes int64)) (string, int64, error) {
	args := []string{
		"--no-playlist", "--no-warnings", "--newline",
		"--socket-timeout", "20",
		"--max-filesize", fmt.Sprintf("%dM", opts.MaxFilesizeMB),
		"--progress-template", "download:VDLPROGRESS|%(progress._percent_str)s|%(progress.total_bytes_estimate)s",
		"-o", filepath.Join(workDir, "%(id)s.%(ext)s"),
	}
	switch format {
	case "audio":
		args = append(args, "-f", "bestaudio", "-x", "--audio-format", "mp3", "--audio-quality", "0")
	default:
		height, err := strconv.Atoi(format)
		if err != nil || height <= 0 {
			return "", 0, &Error{Code: "invalid_format", Message: "Format tidak dikenal"}
		}
		args = append(args,
			"-f", fmt.Sprintf("bestvideo[height<=%d]+bestaudio/best[height<=%d]", height, height),
			"--merge-output-format", "mp4",
		)
	}
	args = append(args, rawURL)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(cctx, opts.YtDlpPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", 0, err
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", 0, &Error{Code: "engine_missing", Message: "yt-dlp tidak ditemukan di server"}
		}
		return "", 0, err
	}

	// Baca progres dari stdout di goroutine terpisah.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			parseProgress(scanner.Text(), onProgress)
		}
	}()

	err = cmd.Wait()
	<-readDone

	if err != nil {
		if ctx.Err() == context.Canceled {
			return "", 0, ErrCanceled
		}
		if cctx.Err() == context.DeadlineExceeded {
			return "", 0, &Error{Code: "timeout", Message: "Waktu unduhan habis", Retryable: true}
		}
		return "", 0, classify(stderr.String())
	}

	path, size, ferr := findOutputFile(workDir)
	if ferr != nil {
		return "", 0, ferr
	}
	return path, size, nil
}

func parseProgress(line string, onProgress func(percent int, totalBytes int64)) {
	if !strings.HasPrefix(line, "VDLPROGRESS|") || onProgress == nil {
		return
	}
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return
	}
	// _percent_str contoh: "  45.2%"
	p := strings.TrimSuffix(strings.TrimSpace(parts[1]), "%")
	f, err := strconv.ParseFloat(p, 64)
	if err != nil {
		return
	}
	var total int64
	if len(parts) >= 3 {
		if v, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64); err == nil {
			total = v
		}
	}
	percent := int(f)
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	onProgress(percent, total)
}

// findOutputFile mencari file hasil unduhan (file terbesar, abaikan .part/.ytdl).
func findOutputFile(workDir string) (string, int64, error) {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		return "", 0, &Error{Code: "storage_error", Message: "Gagal membaca direktori kerja", Retryable: true}
	}
	best := ""
	var bestSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") || strings.HasSuffix(name, ".temp") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			bestSize = info.Size()
			best = name
		}
	}
	if best == "" {
		return "", 0, &Error{Code: "provider_error", Message: "File hasil unduhan tidak ditemukan", Retryable: true}
	}
	return filepath.Join(workDir, best), bestSize, nil
}

func run(ctx context.Context, bin string, args []string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}
