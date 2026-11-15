-- Skema awal MVP: job unduhan + kuota harian per perangkat.

CREATE TABLE IF NOT EXISTS downloads (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id     text        NOT NULL,
    url           text        NOT NULL,
    provider      text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'queued', -- queued|running|completed|failed|canceled|expired
    progress      int         NOT NULL DEFAULT 0,        -- 0..100
    title         text        NOT NULL DEFAULT '',
    thumbnail     text        NOT NULL DEFAULT '',
    duration_secs int         NOT NULL DEFAULT 0,
    format        text        NOT NULL,                  -- '360'|'480'|'720'|'1080'|'audio'
    file_size     bigint      NOT NULL DEFAULT 0,
    storage_path  text        NOT NULL DEFAULT '',       -- nama file di FILES_DIR (relatif)
    filename      text        NOT NULL DEFAULT '',       -- nama file saat dikirim ke pengguna
    error_code    text        NOT NULL DEFAULT '',
    error_message text        NOT NULL DEFAULT '',
    attempts      int         NOT NULL DEFAULT 0,
    next_run_at   timestamptz,                            -- untuk retry dengan backoff
    created_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz,
    expires_at    timestamptz                            -- setelah lewat, file dihapus
);

CREATE INDEX IF NOT EXISTS idx_downloads_status
    ON downloads (status, next_run_at)
    WHERE status IN ('queued', 'running');

CREATE INDEX IF NOT EXISTS idx_downloads_device
    ON downloads (device_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_downloads_expires
    ON downloads (expires_at)
    WHERE expires_at IS NOT NULL AND status = 'completed';

-- Kuota unduhan gratis per perangkat per hari (UTC).
CREATE TABLE IF NOT EXISTS usage_daily (
    device_id text NOT NULL,
    day       date NOT NULL,
    downloads int  NOT NULL DEFAULT 0,
    PRIMARY KEY (device_id, day)
);
