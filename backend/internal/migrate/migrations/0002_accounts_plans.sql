-- Akun, session, plan, langganan + kuota berbasis subject (user atau device).

CREATE TABLE IF NOT EXISTS users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL UNIQUE,
    password_hash text        NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   text        NOT NULL UNIQUE, -- SHA-256 dari token cookie
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions (user_id);

CREATE TABLE IF NOT EXISTS plans (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code            text NOT NULL UNIQUE,
    name            text NOT NULL,
    daily_limit     int  NOT NULL,
    max_height      int  NOT NULL,
    retention_hours int  NOT NULL,
    price_idr       int  NOT NULL DEFAULT 0,
    active          boolean NOT NULL DEFAULT true,
    sort_order      int  NOT NULL DEFAULT 0
);

INSERT INTO plans (code, name, daily_limit, max_height, retention_hours, price_idr, sort_order) VALUES
    ('free', 'Gratis', 3, 720, 6, 0, 0),
    ('pro',  'Pro',    50, 1080, 24, 29000, 1);

CREATE TABLE IF NOT EXISTS subscriptions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id      uuid        NOT NULL REFERENCES plans(id),
    status       text        NOT NULL DEFAULT 'active', -- active|canceled|expired
    provider     text        NOT NULL DEFAULT 'manual', -- manual|midtrans|stripe|telegram
    provider_ref text        NOT NULL DEFAULT '',
    period_start timestamptz NOT NULL DEFAULT now(),
    period_end   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    canceled_at  timestamptz
);

CREATE INDEX IF NOT EXISTS idx_subs_user ON subscriptions (user_id, status);

ALTER TABLE downloads ADD COLUMN IF NOT EXISTS user_id uuid REFERENCES users(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_downloads_user ON downloads (user_id, created_at DESC);

-- Kuota pindah dari kunci device ke subject: 'u:<user_id>' atau 'd:<device_id>'.
CREATE TABLE IF NOT EXISTS usage_daily_new (
    subject   text NOT NULL,
    day       date NOT NULL,
    downloads int  NOT NULL DEFAULT 0,
    PRIMARY KEY (subject, day)
);
INSERT INTO usage_daily_new (subject, day, downloads)
    SELECT 'd:' || device_id, day, downloads FROM usage_daily;
DROP TABLE usage_daily;
ALTER TABLE usage_daily_new RENAME TO usage_daily;
