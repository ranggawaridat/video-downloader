-- Fitur bot Telegram dihapus: bersihkan tabel & kolom pendukungnya.
--
-- Catatan: migrasi 0005 (yang dulu membuat objek-objek ini) sudah dihapus
-- bersama fiturnya. Untuk database yang sempat menjalankannya, migrasi ini
-- yang membersihkan sisa-sisanya (aman dijalankan berulang di DB baru).

DROP TABLE IF EXISTS telegram_link_codes;
DROP TABLE IF EXISTS telegram_links;

-- Kolom khusus bot: penanda notifikasi terkirim dan kunci share file.
ALTER TABLE downloads DROP COLUMN IF EXISTS notified_at;
ALTER TABLE downloads DROP COLUMN IF EXISTS file_key;
