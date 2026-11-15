package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"videodl/backend/internal/billing"
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

// checkout: aktifkan langganan.
// BILLING_MODE=manual (default) mengaktifkan langsung — untuk development
// dan onboarding manual. Mode lain mengembalikan 501 sampai gateway dipasang.
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
	if h.cfg.BillingMode != "manual" {
		writeError(w, http.StatusNotImplemented, "billing_not_configured",
			"Payment gateway belum dikonfigurasi")
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

	if err := billing.ActivateManual(r.Context(), h.pool, u.ID, pl.ID, 30); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "active",
		"plan_code":  pl.Code,
		"period_end": time.Now().AddDate(0, 0, 30).Format(time.RFC3339),
	})
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
