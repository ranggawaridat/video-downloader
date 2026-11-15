// Package billing: plan, langganan, dan entitlement.
//
// Provider pembayaran: saat ini hanya "manual" (aktivasi instan untuk
// development/admin). Midtrans/Stripe/Telegram Stars tinggal ditambahkan
// sebagai implementasi baru tanpa mengubah plan/entitlement di sini.
package billing

import (
	"context"
	"errors"

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

// ActivateManual mengaktifkan langganan instan (mode development/admin).
func ActivateManual(ctx context.Context, pool *pgxpool.Pool, userID, planID string, days int) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, status, provider, period_end)
		VALUES ($1::uuid, $2::uuid, 'active', 'manual', now() + make_interval(days => $3))`,
		userID, planID, days)
	return err
}

// CancelActive menonaktifkan langganan aktif (langsung, tanpa grace period).
func CancelActive(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	_, err := pool.Exec(ctx, `
		UPDATE subscriptions SET status = 'canceled', canceled_at = now()
		WHERE user_id = $1::uuid AND status = 'active'`, userID)
	return err
}
