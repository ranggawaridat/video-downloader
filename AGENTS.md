# AGENTS.md

User-facing messages are in **Indonesian**; code identifiers, logs, and commit messages in English.

## Commands

All Go commands must run from `backend/` (module root), not the repo root:

```bash
cd backend
go build ./... && go vet ./... && gofmt -l .   # full check; gofmt must print nothing
```

There is no test suite yet. Verification = the command above plus manual smoke test via Docker.

```bash
cp .env.example .env
docker compose up -d --build     # postgres + api + worker; then http://localhost:8080
docker compose logs -f api worker
```

Run `docker compose build api worker` separately after code changes — `api` and `worker` are two independently built images from the same Dockerfile; rebuilding only one leaves them inconsistent.

## Architecture (non-obvious)

- Go monolith, two binaries from one codebase: `cmd/api` (HTTP + static web) and `cmd/worker` (job processing). Same Docker image, different `command` in compose.
- **The `downloads` table is the job queue.** No Redis/River. Worker claims jobs with `UPDATE ... WHERE id = (SELECT ... FOR UPDATE SKIP LOCKED)` in `internal/worker/worker.go`. DB row status is the source of truth; never add a parallel queue abstraction without migrating this.
- **Migrations are embedded** in `backend/internal/migrate/migrations/*.sql` and run automatically (with a Postgres advisory lock) at startup by BOTH api and worker. Adding a migration = create `000N_name.sql`; never edit an already-applied file (version is tracked in `schema_migrations`).
- Worker startup re-queues all `running` jobs (`recoverRunning`) — safe only because MVP assumes a single worker instance. If adding worker replicas, this must be revisited.
- File retention (`expires_at` on `downloads`) is set **at job creation** from the user's plan (`plans.retention_hours`). The worker only marks completion; cleanup in `maintain()` deletes expired files. Don't move TTL logic back into the worker.
- Storage is local disk (`FILES_DIR`) with the `.work/` subdir as scratch space. Planned to move to S3/R2; file delivery endpoint checks ownership, don't bypass it.

## Quota & plan model

- Entitlements come from the `plans` table (seeded: `free` 3/day, 720p, 6h retention; `pro` 50/day, 1080p, 24h) — **not** from env vars. Anonymous users get the `free` plan.
- `usage_daily` is keyed by **subject**: `'u:<user_id>'` for logged-in users, `'d:<device_id>'` for anonymous (cookie `vd_device`). The quota increment is a single atomic `INSERT ... ON CONFLICT ... WHERE usage_daily.downloads < limit` — keep it atomic.
- Row ownership uses the `ownershipClause` pattern in `internal/httpapi/downloads.go` (`$1::uuid IS NOT NULL AND user_id = ...`). All user-scoped queries must go through it.

## Billing & Midtrans

- Entitlements come from the `plans` table (seeded: `free` 3/day, 720p, 6h retention; `pro` 50/day, 1080p, 24h) — **not** from env vars. Anonymous users get the `free` plan.
- `usage_daily` is keyed by **subject**: `'u:<user_id>'` for logged-in users, `'d:<device_id>'` for anonymous (cookie `vd_device`). The quota increment is a single atomic `INSERT ... ON CONFLICT ... WHERE usage_daily.downloads < limit` — keep it atomic.
- Row ownership uses the `ownershipClause` pattern in `internal/httpapi/downloads.go` (`$1::uuid IS NOT NULL AND user_id = ...`). All user-scoped queries must go through it.
- Billing modes: `BILLING_MODE=manual` (instant activation, dev/admin fallback) and `BILLING_MODE=midtrans` (Midtrans Snap). `internal/billing` is the seam for adding gateways (e.g. Stripe).
- Midtrans flow: checkout creates a `payments` row (pending, order_id `vd-<hex>-<timestamp>`) → Snap redirect URL → webhook `POST /api/billing/webhook/midtrans` (unauthenticated route, signature SHA-512 verified) → `payments.status='paid'` → `ActivateSubscription` with `provider_ref=order_id` (idempotent — 30-day sub is inserted only if no active sub with that ref exists).
- Webhook idempotency: `webhook_events` table keyed `(provider, event_id)` where event_id = `transaction_id:transaction_status:status_code`. Respond 2xx to Midtrans (including duplicates and unknown orders); 403 only when the signature fails AND the Core API lookup cannot confirm the order.
- Payment status transitions live in `internal/billing` (`MarkPaymentPaid` only flips `pending`→`paid`). Order_id max 50 chars per Midtrans spec.
- Notification signature mismatch is NOT a rejection: the webhook falls back to `Client.GetTransactionStatus` (Core API, server-key authenticated) and trusts that result; only a successful API lookup that disagrees (or fails) yields 403. Nominal is always re-checked against the recorded amount.
- `Handler.StartReconciler` (started from `cmd/api`) sweeps `pending` gateway payments older than 2 min every 2 min and applies the Core API status — this is what recovers payments when the notification never arrives (tunnel down/URL changed). Shared transition logic lives in `billing.ApplyProviderStatus`, used by both webhook and reconciler.
- Snap `callbacks.finish/unfinish/error` point at `PUBLIC_BASE_URL/?payment=<payment_id>`; `web/app.js` reads that query param on load and polls the payment until paid.

## Admin panel

- `/admin` (vanilla JS, `web/admin.*`) gated by `role='admin'` on `users`.
- Admin account is bootstrapped at API startup from `ADMIN_EMAIL`/`ADMIN_PASSWORD` (empty env = skipped). Existing email is promoted to admin, password NOT overwritten.
- Admin API lives under `/api/admin/*` behind `requireAdmin`. Stats use single-row subselects; keep new stats there, not as separate round trips.
- Admin retry (`POST /api/admin/downloads/{id}/retry`) resets `attempts=0` — that's what lets the claim query pick it up again.

## yt-dlp engine

- `yt-dlp` version is **pinned** in `backend/Dockerfile`. Old versions silently break against YouTube/TikTok ("No video formats found") — when downloads fail broadly, bump the pinned version (check https://pypi.org/pypi/yt-dlp/json) and rebuild both images.
- User input is passed to `exec.Command` as args only, never a shell. Error codes come from `classify()` in `internal/ytdlp/ytdlp.go` — the frontend switches on these codes; keep them in sync.
- Do not write tests or CI jobs that hit real YouTube/TikTok URLs.

## Web frontend

`web/` is vanilla HTML/CSS/JS served by the API from `STATIC_DIR` (`../web` locally, `/srv/web` mounted in compose). No build step, no bundler — don't add a framework. Quality chips read `max_height` from `/api/metadata`, which is plan-based.

Every render helper must be null-safe (`const el = $("#x"); if (!el) return;`) and `init()` runs each refresh step isolated (`await refreshX().catch(() => {})`). A single missing DOM id used to abort the whole chain, leaving sections (e.g. the pricing list) silently empty. Keep `#quota` (header) and `#plan-list` in sync with `app.js`. `renderPlans()` re-renders from `plansCache`, so calling it without arguments after `refreshMe()` is safe and keeps the "Paket aktif" state correct.

## Git

- Trunk-based: short-lived branches off `main`, Conventional Commits (`feat:`, `fix:`, `chore:`), squash-merge style commits.
- `docker compose up` recreates containers with a few seconds of downtime — acceptable for this stage.
