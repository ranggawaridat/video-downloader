package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"videodl/backend/internal/billing"
	"videodl/backend/internal/midtrans"
)

// listPlans: daftar paket untuk halaman harga.
func (h *Handler) listPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := billing.ListActivePlans(r.Context(), h.pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if plans == nil {
		plans = []billing.Plan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": plans})
}

// newOrderID: "vd-<16 hex>-<unix 10 digit>" (maks 50 karakter Midtrans).
func newOrderID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "vd-" + hex.EncodeToString(b[:]) + "-" + time.Now().UTC().Format("010206150405")
}

// checkout: aktifkan langganan.
// BILLING_MODE=midtrans → buat payment pending + redirect URL Snap.
// BILLING_MODE=manual (default) → aktivasi instan untuk development/admin.
func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanCode string `json:"plan_code"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu untuk berlangganan")
		return
	}

	pl, err := billing.PlanByCode(r.Context(), h.pool, req.PlanCode)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (pl == nil || pl.PriceIDR <= 0)) {
		writeError(w, http.StatusBadRequest, "invalid_plan", "Paket tidak ditemukan")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	exists, err := billing.HasActiveSubscription(r.Context(), h.pool, u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if exists {
		writeError(w, http.StatusConflict, "already_subscribed", "Kamu sudah punya langganan aktif")
		return
	}

	switch h.cfg.BillingMode {
	case "midtrans":
		h.checkoutMidtrans(w, r, u.ID, pl)
	default:
		h.checkoutManual(w, r, u.ID, pl)
	}
}

// checkoutMidtrans: buat payment pending, minta Snap transaction,
// simpan redirect URL. Webhook yang akan mengaktifkan langganan.
func (h *Handler) checkoutMidtrans(w http.ResponseWriter, r *http.Request, userID string, pl *billing.Plan) {
	if !h.midtrans.Configured() {
		writeError(w, http.StatusNotImplemented, "billing_not_configured",
			"Payment gateway belum dikonfigurasi (MIDTRANS_SERVER_KEY kosong)")
		return
	}

	orderID := newOrderID()
	pm, err := billing.CreatePayment(r.Context(), h.pool, userID, pl.ID, "midtrans", orderID, pl.PriceIDR)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	// Halaman tujuan setelah pembayaran di Snap — kembali ke web kita,
	// bukan layar Snap yang buntu.
	var finish, unfinish, errURL string
	if base := strings.TrimRight(h.cfg.PublicBaseURL, "/"); base != "" {
		back := base + "/?payment=" + pm.ID
		finish, unfinish, errURL = back, back, back
	}

	redirect, _, err := h.midtrans.CreateSnapTransaction(r.Context(), midtrans.SnapTx{
		OrderID:       orderID,
		GrossAmount:   pl.PriceIDR,
		CustomerEmail: userFromContext(r.Context()).Email,
		ItemID:        pl.Code,
		ItemName:      "Langganan " + pl.Name + " 30 hari",
		FinishURL:     finish,
		UnfinishURL:   unfinish,
		ErrorURL:      errURL,
	})
	if err != nil {
		h.log.Error("midtrans snap failed", "order_id", orderID, "err", err)
		_ = billing.SetPaymentStatus(r.Context(), h.pool, pm.ID, "failed")
		writeError(w, http.StatusBadGateway, "gateway_error",
			"Gagal membuat transaksi pembayaran, coba lagi")
		return
	}
	if err := billing.UpdatePaymentRedirect(r.Context(), h.pool, pm.ID, redirect); err != nil {
		h.log.Error("save redirect failed", "err", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"payment_id":  pm.ID,
		"status":      "pending",
		"payment_url": redirect,
	})
}

// checkoutManual: aktivasi instan + catat payment paid (riwayat).
func (h *Handler) checkoutManual(w http.ResponseWriter, r *http.Request, userID string, pl *billing.Plan) {
	if err := billing.ActivateManual(r.Context(), h.pool, userID, pl.ID, 30); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if err := billing.RecordManualPayment(r.Context(), h.pool, userID, pl.ID, pl.PriceIDR); err != nil {
		h.log.Error("record manual payment failed", "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "active",
		"plan_code":  pl.Code,
		"period_end": time.Now().AddDate(0, 0, 30).Format(time.RFC3339),
	})
}

// paymentStatus: polling status payment milik sendiri (frontend).
func (h *Handler) paymentStatus(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu")
		return
	}
	pm, err := billing.PaymentByID(r.Context(), h.pool, chi.URLParam(r, "id"), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if pm == nil {
		writeError(w, http.StatusNotFound, "not_found", "Pembayaran tidak ditemukan")
		return
	}
	writeJSON(w, http.StatusOK, pm)
}

// midtransWebhook: notifikasi Midtrans (Notification API).
// Route ini TIDAK lewat session middleware auth — dipanggil server Midtrans.
// Signature SHA-512 diverifikasi; jika tidak cocok (mis. dashboard memakai
// server key berbeda), status dipastikan lewat Core API terautentikasi.
// Selalu balas 2xx bila notifikasi berasal dari Midtrans yang sah.
func (h *Handler) midtransWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	var n struct {
		OrderID     string `json:"order_id"`
		StatusCode  string `json:"status_code"`
		GrossAmount string `json:"gross_amount"`
		Signature   string `json:"signature_key"`
		TxnStatus   string `json:"transaction_status"`
		FraudStatus string `json:"fraud_status"`
		TxnID       string `json:"transaction_id"`
	}
	if err := json.Unmarshal(body, &n); err != nil || n.OrderID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	if !h.midtrans.VerifySignature(n.OrderID, n.StatusCode, n.GrossAmount, n.Signature) {
		// Signature tidak cocok TIDAK langsung ditolak: server key yang dipakai
		// menandatangani notifikasi bisa berbeda dari yang kita simpan. Kita
		// tanyakan status transaksinya langsung ke Midtrans memakai server key
		// kita sendiri (terautentikasi) dan percayai jawabannya.
		st, verr := h.midtrans.GetTransactionStatus(r.Context(), n.OrderID)
		if verr != nil || st == nil || st.OrderID != n.OrderID {
			h.log.Warn("midtrans webhook rejected",
				"order_id", n.OrderID, "err", verr)
			writeError(w, http.StatusForbidden, "invalid_signature", "Signature tidak valid")
			return
		}
		h.log.Warn("midtrans webhook signature mismatch — verified via core api",
			"order_id", n.OrderID,
			"txn_status", st.TransactionStatus,
			"received_sig", maskTail(n.Signature),
			"expected_sig", maskTail(h.midtrans.Signature(n.OrderID, n.StatusCode, n.GrossAmount)))
		n.TxnStatus = st.TransactionStatus
		n.FraudStatus = st.FraudStatus
		n.StatusCode = st.StatusCode
		n.GrossAmount = st.GrossAmount
		n.TxnID = st.TransactionID
	}

	// Idempotensi: event yang sama hanya diproses sekali.
	eventID := n.TxnID + ":" + n.TxnStatus + ":" + n.StatusCode
	isNew, err := billing.RecordWebhookEvent(r.Context(), h.pool, "midtrans", eventID, body)
	if err != nil {
		h.log.Error("record webhook event failed", "err", err)
		w.WriteHeader(http.StatusOK) // Midtrans akan retry
		return
	}
	if !isNew {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	pm, err := billing.PaymentByRef(r.Context(), h.pool, "midtrans", n.OrderID)
	if err != nil || pm == nil {
		h.log.Warn("midtrans webhook unknown order", "order_id", n.OrderID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "unknown_order"})
		return
	}

	// Jangan percaya nominal dari notifikasi: harus sama dengan nilai tercatat.
	if amt, perr := strconv.ParseFloat(n.GrossAmount, 64); perr != nil || int(amt+0.5) != pm.AmountIDR {
		h.log.Warn("midtrans webhook amount mismatch",
			"order_id", n.OrderID, "got", n.GrossAmount, "want", pm.AmountIDR)
		writeJSON(w, http.StatusOK, map[string]string{"status": "amount_mismatch"})
		return
	}

	activated, err := billing.ApplyProviderStatus(r.Context(), h.pool, pm, n.TxnStatus, n.FraudStatus, subscriptionDays)
	if err != nil {
		h.log.Error("apply provider status failed", "order_id", n.OrderID, "err", err)
		// Tetap 200: Midtrans tidak akan retry untuk kegagalan sisi kita,
		// tapi reconciler akan menyelesaikannya.
		writeJSON(w, http.StatusOK, map[string]string{"status": "retry_later"})
		return
	}
	if activated {
		h.log.Info("subscription activated via midtrans", "order_id", n.OrderID)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// maskTail menyamarkan nilai sensitif untuk log (hanya 8 karakter awal).
func maskTail(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:8] + "…"
}

func (h *Handler) cancelSubscription(w http.ResponseWriter, r *http.Request) {
	u := userFromContext(r.Context())
	if u == nil {
		writeError(w, http.StatusUnauthorized, "login_required", "Masuk dulu")
		return
	}
	if err := billing.CancelActive(r.Context(), h.pool, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}
