# VidGrab — Video Downloader (MVP)

Video downloader multi-situs (YouTube, TikTok, dll via yt-dlp) dengan backend Go,
job queue berbasis PostgreSQL, dan web frontend sederhana (vanilla HTML/CSS/JS).

## Arsitektur MVP

```text
[Web Browser] ──> [API Go :8080] ──> [PostgreSQL] <── [Worker Go] ──> yt-dlp + ffmpeg ──> file
                        │                │  (tabel downloads = source of truth + job queue)
                        └── static web ──┘
```

- **API** menerima request, validasi URL (anti-SSRF), cek kuota, insert job.
- **Worker** mengambil job `queued` dengan `FOR UPDATE SKIP LOCKED`, menjalankan
  yt-dlp, memperbarui progres, lalu menandai `completed`.
- **File** disimpan di volume `files` dan dihapus otomatis setelah TTL
  (`FILE_TTL_HOURS`, default 6 jam).
- **Kuota & plan** dari tabel `plans`: free (3/hari, 720p, retensi 6 jam) dan
  pro (50/hari, 1080p, retensi 24 jam) — bisa diubah langsung di DB/admin nanti.
- **Akun**: email + password (Argon2id), session cookie HttpOnly 30 hari.
  Riwayat unduhan anonim di browser otomatis dipindah ke akun saat login/daftar.
- **Billing** mode `manual`: checkout mengaktifkan langganan instan (30 hari).
  Ganti ke Midtrans/Stripe/Telegram Stars dengan menambah implementasi provider.

## Menjalankan (Docker Compose)

```bash
cp .env.example .env          # sesuaikan bila perlu
docker compose up -d --build
```

Buka http://localhost:8080

Stop: `docker compose down` (tambah `-v` untuk menghapus data).

## Menjalankan lokal tanpa Docker

Kebutuhan: Go 1.23+, PostgreSQL, yt-dlp, ffmpeg di PATH.

```bash
# terminal 1 — database (contoh docker)
docker run -d --name videodl-db -p 5432:5432 \
  -e POSTGRES_DB=videodl -e POSTGRES_USER=videodl -e POSTGRES_PASSWORD=videodl \
  postgres:16-alpine

# terminal 2 — api
cd backend
go run ./cmd/api          # default DATABASE_URL localhost, STATIC_DIR ../web

# terminal 3 — worker
cd backend
go run ./cmd/worker
```

## Endpoint

| Method | Path | Keterangan |
|---|---|---|
| GET | `/api/health` | Health check (ping DB) |
| GET | `/api/me` | Identitas + plan aktif + kuota hari ini |
| GET | `/api/plans` | Daftar paket (harga & benefit) |
| POST | `/api/auth/register` | `{email, password}` — daftar + langsung login |
| POST | `/api/auth/login` | `{email, password}` |
| POST | `/api/auth/logout` | Hapus session |
| POST | `/api/billing/checkout` | `{plan_code}` — aktifkan langganan (mode `manual`) |
| POST | `/api/billing/cancel` | Batalkan langganan aktif |
| POST | `/api/metadata` | `{url}` → judul, thumbnail, durasi, daftar resolusi |
| POST | `/api/downloads` | `{url, format}` → buat job (`360`/`480`/`720`/`1080`/`audio`) |
| GET | `/api/downloads` | Riwayat (user login atau device anonim) |
| GET | `/api/downloads/{id}` | Status + progres |
| DELETE | `/api/downloads/{id}` | Batalkan job |
| GET | `/api/files/{id}` | File hasil (hanya pemilik unduhan) |

Format error: `{"error": {"code": "...", "message": "..."}}`.

## Konfigurasi (env)

| Variabel | Default | Keterangan |
|---|---|---|
| `ADDR` | `:8080` | Alamat listen API |
| `DATABASE_URL` | — | DSN PostgreSQL |
| `FILES_DIR` | `./data/files` | Penyimpanan file |
| `STATIC_DIR` | `../web` | Direktori frontend |
| `BILLING_MODE` | `manual` | `manual` = aktivasi langganan instan (dev); gateway lain menyusul |
| `MAX_FILESIZE_MB` | `500` | Batas ukuran file |
| `JOB_TIMEOUT_MINUTES` | `20` | Timeout per job |
| `MAX_ATTEMPTS` | `3` | Percobaan ulang per job |
| `WORKER_CONCURRENCY` | `2` | Job paralel per worker |
| `YTDLP_PATH` / `FFMPEG_PATH` | `yt-dlp` / `ffmpeg` | Path binary engine |

## Struktur

```text
backend/
  cmd/api/          entrypoint API
  cmd/worker/       entrypoint worker
  internal/
    config/         env config
    db/             koneksi pgx
    migrate/        migrasi SQL embedded (advisory lock)
    validate/       validasi URL + anti-SSRF
    device/         identitas perangkat (cookie)
    ytdlp/          wrapper yt-dlp (metadata, download, klasifikasi error)
    worker/         job queue FOR UPDATE SKIP LOCKED + maintenance
    httpapi/        handler HTTP + static
web/                frontend vanilla
docker-compose.yml  postgres + api + worker
```

## Error umum

| Kode | Arti |
|---|---|
| `invalid_url` | URL tidak valid / mengarah ke jaringan internal |
| `unsupported_url` | Situs belum didukung yt-dlp |
| `private_content` | Video privat / perlu login |
| `too_large` | Melebihi `MAX_FILESIZE_MB` |
| `quota_exceeded` | Kuota harian habis |
| `timeout` / `provider_error` | Gangguan sementara — otomatis di-retry |

## Batasan MVP & langkah berikutnya

- **Billing manual** — pembayaran nyata (Midtrans/Stripe/Telegram Stars) menyusul;
  struktur provider sudah disiapkan di `internal/billing`.
- **Login Telegram/Google** — belum; akun email dulu.
- **Queue sederhana berbasis DB** — siap ditukar ke River/asynq saat skala naik.
- **Storage lokal** — nanti dipindah ke S3/R2 untuk multi-worker.
- **Tanpa HTTPS** — pasang reverse proxy (Caddy/Nginx) di depan untuk produksi.
- **yt-dlp di-pin** di `backend/Dockerfile` — update versinya secara berkala.
- **Hukum**: hanya untuk konten yang berhak diunduh; sediakan ToS/DMCA sebelum publik.
