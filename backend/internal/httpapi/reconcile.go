package httpapi

import (
	"context"
	"time"

	"videodl/backend/internal/billing"
)

const (
	// reconcileInterval jarak pemeriksaan payment yang menggantung.
	reconcileInterval = 2 * time.Minute
	// reconcileMinAge lewati payment yang baru dibuat (beri waktu webhook).
	reconcileMinAge = 2 * time.Minute
	// reconcileBatch batas payment yang diperiksa per putaran.
	reconcileBatch = 20
	// subscriptionDays durasi langganan per pembayaran.
	subscriptionDays = 30
)

// StartReconciler menjalankan loop latar belakang yang memulihkan pembayaran
// nyangkut: notifikasi Midtrans bisa tidak sampai (URL publik berubah, tunnel
// mati, signature beda), jadi payment yang masih pending ditanyakan langsung
// ke Core API Midtrans memakai server key kita.
func (h *Handler) StartReconciler(ctx context.Context) {
	if h.cfg.BillingMode != "midtrans" || !h.midtrans.Configured() {
		return
	}
	go func() {
		t := time.NewTicker(reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.reconcilePending(ctx)
			}
		}
	}()
}

func (h *Handler) reconcilePending(ctx context.Context) {
	payments, err := billing.PendingPayments(ctx, h.pool, reconcileMinAge, reconcileBatch)
	if err != nil {
		h.log.Error("reconcile: list pending failed", "err", err)
		return
	}
	for _, pm := range payments {
		if ctx.Err() != nil {
			return
		}
		st, err := h.midtrans.GetTransactionStatus(ctx, pm.ProviderRef)
		if err != nil {
			// Transaksi yang belum dibuka di Snap bisa 404 — bukan masalah.
			h.log.Debug("reconcile: status lookup failed",
				"order_id", pm.ProviderRef, "err", err)
			continue
		}
		activated, err := billing.ApplyProviderStatus(ctx, h.pool, pm,
			st.TransactionStatus, st.FraudStatus, subscriptionDays)
		if err != nil {
			h.log.Error("reconcile: apply failed",
				"order_id", pm.ProviderRef, "err", err)
			continue
		}
		if activated {
			h.log.Info("subscription activated via reconcile", "order_id", pm.ProviderRef)
			continue
		}
		switch st.TransactionStatus {
		case "deny", "cancel", "expire", "refund", "partial_refund":
			h.log.Info("reconcile: payment closed",
				"order_id", pm.ProviderRef, "txn_status", st.TransactionStatus)
		}
	}
}
