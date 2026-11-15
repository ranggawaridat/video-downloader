-- Bot Telegram: ikatan chat ↔ akun web, kode link sekali pakai,
-- plus kolom pendukung notifikasi & link file.

-- Status download yang sudah diberitahukan ke chat Telegram oleh bot.
ALTER TABLE downloads ADD COLUMN IF NOT EXISTS notified_at timestamptz;

-- Kunci share file: link /api/files/{id}?k=<file_key> boleh dibuka
-- tanpa session (untuk file besar yang tidak bisa dikirim via sendDocument).
ALTER TABLE downloads ADD COLUMN IF NOT EXISTS file_key text NOT NULL DEFAULT '';

-- Chat Telegram boleh terikat ke akun web (user_id NULL = anonim,
-- identitas device "tg:<chat_id>").
CREATE TABLE IF NOT EXISTS telegram_links (
    chat_id    bigint PRIMARY KEY,
    user_id    uuid REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Kode sekali pakai untuk /link di bot (dibuat dari web, 10 menit).
CREATE TABLE IF NOT EXISTS telegram_link_codes (
    code       text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
