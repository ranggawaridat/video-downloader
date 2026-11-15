-- Pembayaran (gateway) + log webhook untuk idempotensi.

CREATE TABLE IF NOT EXISTS payments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id      uuid        NOT NULL REFERENCES plans(id),
    provider     text        NOT NULL,                   -- midtrans|manual
    provider_ref text        NOT NULL DEFAULT '',        -- order_id Midtrans
    amount_idr   int         NOT NULL DEFAULT 0,
    status       text        NOT NULL DEFAULT 'pending', -- pending|paid|failed|expired|refunded
    redirect_url text        NOT NULL DEFAULT '',        -- Snap redirect URL
    raw          jsonb       NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    paid_at      timestamptz
);

-- Satu order_id unik per provider (kosong = aktivasi manual, boleh duplikat).
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_ref
    ON payments (provider, provider_ref)
    WHERE provider_ref <> '';

CREATE INDEX IF NOT EXISTS idx_payments_user ON payments (user_id, created_at DESC);

-- Idempotensi webhook: event yang sama tidak diproses dua kali.
CREATE TABLE IF NOT EXISTS webhook_events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider   text        NOT NULL,
    event_id   text        NOT NULL,
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, event_id)
);
