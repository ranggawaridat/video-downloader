// Package billing: plan, langganan, dan entitlement.
//
// Provider pembayaran: saat ini hanya "manual" (aktivasi instan untuk
// development/admin). Midtrans/Stripe/Telegram Stars tinggal ditambahkan
// sebagai implementasi baru tanpa mengubah plan/entitlement di sini.
package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Plan mendefinisikan entitlement: kuota harian, resolusi maks, retensi file.
type Plan struct {
	ID             string `json:"-"`
	Code           string `json:"code"`
	Name           string `json:"name"`
	DailyLimit     int    `json:"daily_limit"`
	MaxHeight      int    `json:"max_height"`
	RetentionHours int    `json:"retention_hours"`
	PriceIDR       int    `json:"price_idr"`
}

const planCols = "p.id::text, p.code, p.name, p.daily_limit, p.max_height, p.retention_hours, p.price_idr"

func scanPlan(row pgx.Row) (*Plan, error) {
	var p Plan
	if err := row.Scan(&p.ID, &p.Code, &p.Name, &p.DailyLimit, &p.MaxHeight, &p.RetentionHours, &p.PriceIDR); err != nil {
		return nil, err
	}
	return &p, nil
}

// FreePlan mengembalikan plan gratis (fallback semua pengguna tanpa langganan).
func FreePlan(ctx context.Context, pool *pgxpool.Pool) (*Plan, error) {
	return planByCode(ctx, pool, "free")
}

func planByCode(ctx context.Context, pool *pgxpool.Pool, code string) (*Plan, error) {
	p, err := scanPlan(pool.QueryRow(ctx,
		"SELECT "+planCols+" FROM plans p WHERE p.code = $1 AND p.active", code))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// PlanByCode mencari plan berbayar untuk checkout.
func PlanByCode(ctx context.Context, pool *pgxpool.Pool, code string) (*Plan, error) {
	return planByCode(ctx, pool, code)
}

// ListActivePlans daftar plan untuk halaman harga, termurah dulu.
func ListActivePlans(ctx context.Context, pool *pgxpool.Pool) ([]Plan, error) {
	rows, err := pool.Query(ctx,
		"SELECT "+planCols+" FROM plans p WHERE p.active ORDER BY p.sort_order, p.price_idr")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		var p Plan
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.DailyLimit, &p.MaxHeight, &p.RetentionHours, &p.PriceIDR); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActivePlanForUser: plan dari langganan aktif, atau free.
func ActivePlanForUser(ctx context.Context, pool *pgxpool.Pool, userID string) (*Plan, error) {
	p, err := scanPlan(pool.QueryRow(ctx, `
		SELECT `+planCols+`
		FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		WHERE s.user_id = $1::uuid AND s.status = 'active' AND s.period_end > now()
		ORDER BY s.period_end DESC
		LIMIT 1`, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return FreePlan(ctx, pool)
	}
	return p, err
}

// HasActiveSubscription mengecek apakah user sudah punya langganan berjalan.
func HasActiveSubscription(ctx context.Context, pool *pgxpool.Pool, userID string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM subscriptions
			WHERE user_id = $1::uuid AND status = 'active' AND period_end > now()
		)`, userID).Scan(&exists)
	return exists, err
}

// ActivateSubscription mengaktifkan langganan berbayar 30 hari.
// provider: "manual" (admin/dev) atau "midtrans" (webhook setelah paid).
// Idempotent: jika payment sudah pernah mengaktifkan langganan (ada
// subscription dengan provider_ref yang sama), tidak dibuat lagi.
func ActivateSubscription(ctx context.Context, pool *pgxpool.Pool, userID, planID, provider, providerRef string, days int) error {
	if providerRef != "" {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM subscriptions
				WHERE provider_ref = $1 AND status = 'active'
			)`, providerRef).Scan(&exists); err == nil && exists {
			return nil
		}
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, provider, provider_ref, period_end)
		VALUES ($1::uuid, $2::uuid, 'active', $3, $4, now() + make_interval(days => $5))`,
		userID, planID, provider, providerRef, days)
	return err
}

// ActivateManual mengaktifkan langganan instan (mode development/admin).
func ActivateManual(ctx context.Context, pool *pgxpool.Pool, userID, planID string, days int) error {
	return ActivateSubscription(ctx, pool, userID, planID, "manual", "", days)
}

// ---------- payments ----------

// Payment satu transaksi pembayaran ke gateway (atau aktivasi manual).
type Payment struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	PlanID      string     `json:"-"`
	PlanCode    string     `json:"plan_code"`
	Provider    string     `json:"provider"`
	ProviderRef string     `json:"provider_ref"`
	AmountIDR   int        `json:"amount_idr"`
	Status      string     `json:"status"`
	RedirectURL string     `json:"redirect_url"`
	CreatedAt   time.Time  `json:"created_at"`
	PaidAt      *time.Time `json:"paid_at,omitempty"`
}

const paymentCols = `
	pm.id::text, pm.user_id::text, pm.plan_id::text, p.code, pm.provider, pm.provider_ref,
	pm.amount_idr, pm.status, pm.redirect_url, pm.created_at, pm.paid_at`

func scanPayment(row pgx.Row) (*Payment, error) {
	var m Payment
	if err := row.Scan(&m.ID, &m.UserID, &m.PlanID, &m.PlanCode, &m.Provider, &m.ProviderRef,
		&m.AmountIDR, &m.Status, &m.RedirectURL, &m.CreatedAt, &m.PaidAt); err != nil {
		return nil, err
	}
	return &m, nil
}

// CreatePayment membuat payment pending sebelum redirect ke gateway.
// providerRef = order_id (unik per provider, kosong untuk manual).
func CreatePayment(ctx context.Context, pool *pgxpool.Pool, userID, planID, provider, providerRef string, amountIDR int) (*Payment, error) {
	m, err := scanPayment(pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO payments (user_id, plan_id, provider, provider_ref, amount_idr)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			RETURNING *
		)
		SELECT `+paymentCols+`
		FROM ins pm JOIN plans p ON p.id = pm.plan_id`,
		userID, planID, provider, providerRef, amountIDR))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// PaymentByID mengambil payment milik user tertentu (owner check di caller).
func PaymentByID(ctx context.Context, pool *pgxpool.Pool, id, userID string) (*Payment, error) {
	m, err := scanPayment(pool.QueryRow(ctx,
		"SELECT "+paymentCols+" FROM payments pm JOIN plans p ON p.id = pm.plan_id "+
			"WHERE pm.id = $1::uuid AND pm.user_id = $2::uuid", id, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// PaymentByRef mencari payment berdasarkan order_id gateway (untuk webhook).
func PaymentByRef(ctx context.Context, pool *pgxpool.Pool, provider, providerRef string) (*Payment, error) {
	m, err := scanPayment(pool.QueryRow(ctx,
		"SELECT "+paymentCols+" FROM payments pm JOIN plans p ON p.id = pm.plan_id "+
			"WHERE pm.provider = $1 AND pm.provider_ref = $2", provider, providerRef))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// UpdatePaymentRedirect menyimpan redirect URL Snap.
func UpdatePaymentRedirect(ctx context.Context, pool *pgxpool.Pool, id, redirectURL string) error {
	_, err := pool.Exec(ctx,
		"UPDATE payments SET redirect_url = $1 WHERE id = $2::uuid", redirectURL, id)
	return err
}

// SetPaymentStatus mengubah status payment (+paid_at saat paid).
func SetPaymentStatus(ctx context.Context, pool *pgxpool.Pool, id, status string) error {
	_, err := pool.Exec(ctx, `
		UPDATE payments SET status = $1,
		       paid_at = CASE WHEN $1 = 'paid' THEN now() ELSE paid_at END
		WHERE id = $2::uuid`, status, id)
	return err
}

// MarkPaymentPaid set status paid; mengembalikan false jika sudah paid
// (idempotensi: webhook bisa datang lebih dari sekali).
func MarkPaymentPaid(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	ct, err := pool.Exec(ctx, `
		UPDATE payments SET status = 'paid', paid_at = now()
		WHERE id = $1::uuid AND status = 'pending'`, id)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// RecordWebhookEvent menyimpan event gateway untuk idempotensi.
// Mengembalikan false jika event sudah pernah diproses.
func RecordWebhookEvent(ctx context.Context, pool *pgxpool.Pool, provider, eventID string, payload []byte) (bool, error) {
	ct, err := pool.Exec(ctx, `
		INSERT INTO webhook_events (provider, event_id, payload)
		VALUES ($1, $2, $3::jsonb)
		ON CONFLICT (provider, event_id) DO NOTHING`, provider, eventID, payload)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// RecordManualPayment: catat aktivasi manual sebagai payment paid
// agar riwayat pembayaran tetap terlacak.
func RecordManualPayment(ctx context.Context, pool *pgxpool.Pool, userID, planID string, amountIDR int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO payments (user_id, plan_id, provider, provider_ref, amount_idr, status, paid_at)
		VALUES ($1::uuid, $2::uuid, 'manual', '', $3, 'paid', now())`,
		userID, planID, amountIDR)
	return err
}

// PendingPayments: payment gateway yang masih menggantung dan sudah berumur
// lebih dari minAge — kandidat rekonsiliasi bila notifikasi tidak sampai.
func PendingPayments(ctx context.Context, pool *pgxpool.Pool, minAge time.Duration, limit int) ([]*Payment, error) {
	rows, err := pool.Query(ctx, `
		SELECT `+paymentCols+`
		FROM payments pm JOIN plans p ON p.id = pm.plan_id
		WHERE pm.status = 'pending'
		  AND pm.provider <> 'manual'
		  AND pm.provider_ref <> ''
		  AND pm.created_at < now() - make_interval(secs => $1)
		ORDER BY pm.created_at
		LIMIT $2`, int(minAge.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Payment
	for rows.Next() {
		m, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ApplyProviderStatus menerapkan status transaksi gateway pada satu payment,
// dan mengaktifkan langganan bila sudah lunas. Dipakai bersama oleh webhook
// dan reconciler supaya aturannya satu tempat.
// Mengembalikan true bila langganan baru saja diaktifkan.
func ApplyProviderStatus(ctx context.Context, pool *pgxpool.Pool, pm *Payment, txnStatus, fraudStatus string, days int) (bool, error) {
	if pm == nil {
		return false, nil
	}
	paid := txnStatus == "settlement" ||
		(txnStatus == "capture" && (fraudStatus == "" || fraudStatus == "accept"))

	switch {
	case paid:
		isNewPaid, err := MarkPaymentPaid(ctx, pool, pm.ID)
		if err != nil {
			return false, err
		}
		if !isNewPaid {
			return false, nil // sudah pernah paid: jangan aktifkan dua kali
		}
		if err := ActivateSubscription(ctx, pool, pm.UserID, pm.PlanID, pm.Provider, pm.ProviderRef, days); err != nil {
			return false, err
		}
		return true, nil

	case txnStatus == "pending":
		return false, nil // masih menunggu, tidak ada perubahan

	case txnStatus == "deny", txnStatus == "cancel", txnStatus == "expire":
		if pm.Status != "pending" {
			return false, nil // jangan turunkan status payment yang sudah paid
		}
		return false, SetPaymentStatus(ctx, pool, pm.ID, "failed")

	case txnStatus == "refund", txnStatus == "partial_refund":
		return false, SetPaymentStatus(ctx, pool, pm.ID, "refunded")
	}
	return false, nil
}

// CancelActive menonaktifkan langganan aktif (langsung, tanpa grace period).
func CancelActive(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	_, err := pool.Exec(ctx, `
		UPDATE subscriptions SET status = 'canceled', canceled_at = now()
		WHERE user_id = $1::uuid AND status = 'active'`, userID)
	return err
}
