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

Buka http://localhost:8080 — frontend pengguna.
Buka http://localhost:8080/admin — panel admin (akun dari `ADMIN_EMAIL`/`ADMIN_PASSWORD`).

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
| GET | `/api/admin/stats/overview` | **Admin** — statistik dashboard |
| GET | `/api/admin/users` | **Admin** — daftar user (query `q=`) |
| POST | `/api/admin/users/{id}/grant` | **Admin** — aktivasi manual langganan |
| GET | `/api/admin/downloads` | **Admin** — semua job (query `status=`) |
| POST | `/api/admin/downloads/{id}/retry` | **Admin** — requeue job gagal/canceled |
| PATCH | `/api/admin/plans/{code}` | **Admin** — ubah benefit plan |
| POST | `/api/billing/checkout` | `{plan_code}` — mode `manual`: aktivasi instan; mode `midtrans`: buat Snap payment |
| GET | `/api/billing/payments/{id}` | Status payment milik sendiri (polling frontend) |
| POST | `/api/billing/webhook/midtrans` | Notifikasi Midtrans (verifikasi signature, idempotent) |
| POST | `/api/billing/cancel` | Batalkan langganan aktif |
| POST | `/api/auth/telegram/link` | Buat kode sekali pakai (10 menit) untuk `/link` di bot |
| DELETE | `/api/auth/telegram/link` | Lepas ikatan chat Telegram dari akun |
| GET | `/api/auth/telegram/status` | Status ikatan chat (untuk UI web) |
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
| `BILLING_MODE` | `manual` | `manual` = aktivasi instan (dev/admin); `midtrans` = Midtrans Snap |
| `MIDTRANS_SERVER_KEY` | — | Server key Midtrans (dashboard → Settings → Access Keys) |
| `MIDTRANS_IS_PRODUCTION` | `false` | `true` = produksi (`app.midtrans.com`), default sandbox |
| `TELEGRAM_BOT_TOKEN` | — | Token @BotFather (kosong = bot idle) |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | URL publik untuk link file dari bot |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | — | Bootstrap akun admin saat API start (kosong = lewati) |
| `MAX_FILESIZE_MB` | `500` | Batas ukuran file |
| `JOB_TIMEOUT_MINUTES` | `20` | Timeout per job |
| `MAX_ATTEMPTS` | `3` | Percobaan ulang per job |
| `WORKER_CONCURRENCY` | `2` | Job paralel per worker |
| `YTDLP_PATH` / `FFMPEG_PATH` | `yt-dlp` / `ffmpeg` | Path binary engine |

## Bot Telegram

Service `bot` (`cmd/telegrambot`, env `TELEGRAM_BOT_TOKEN`) adalah client
kedua yang memakai logika backend yang sama (tabel `downloads` sebagai
antrean, kuota atomis di `internal/jobs`). Set `TELEGRAM_BOT_TOKEN` di
`.env` lalu `docker compose up -d bot`.

- **Tanpa ikatan**: chat = perangkat anonim `tg:<chat_id>` — kuota free plan.
- **Ikat ke akun web**: situs → panel Telegram → "Ambil kode" → kirim
  `/link <kode>` ke bot. Setelah itu chat memakai plan & kuota akun.
- Perintah: `/download <link>` (pilih kualitas via inline keyboard),
  `/status`, `/unlink`, atau langsung kirim link.
- **File ≤ 45 MB** dikirim langsung sebagai dokumen; **file besar** dikirim
  sebagai link `/api/files/{id}?k=<file_key>` (kunci share sekali-generate,
  divalidasi `serveFile`).
- Notifikasi hasil job di-poll tiap 5 detik (kolom `notified_at` mencegah
  dobel). Hanya job yang dibuat via bot yang dinotifikasi.

## Pembayaran (Midtrans Snap)

Mode `BILLING_MODE=midtrans` mengaktifkan pembayaran nyata via Midtrans Snap
(QRIS, GoPay, VA, kartu). Tanpa konfigurasi ini, mode `manual` tetap jalan
sebagai fallback.

1. Daftar/akses [dashboard Midtrans](https://dashboard.midtrans.com), ambil
   **Server Key** dari *Settings → Access Keys* (pakai *Sandbox* untuk uji).
2. Isi `.env`:
   ```env
   BILLING_MODE=midtrans
   MIDTRANS_SERVER_KEY=SB-Mid-server-xxxx
   MIDTRANS_IS_PRODUCTION=false
   ```
3. Set **Notification URL** di *Settings → Configuration*:
   `https://<domain>/api/billing/webhook/midtrans`
   (endpoint memverifikasi signature SHA-512 dan idempotent; Midtrans
   mengharapkan respons 2xx).
4. Testing webhook lokal butuh URL publik — pakai tunnel:
   `cloudflared tunnel --url http://localhost:8080` lalu arahkan Notification
   URL ke domain tunnel.

Alur: checkout → payment `pending` + redirect URL Snap → user bayar →
webhook `settlement` → payment `paid` → langganan 30 hari aktif otomatis.
Aktivasi idempotent via `provider_ref` (order_id); frontend mem-poll
`GET /api/billing/payments/{id}` setelah membuka Snap.

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
