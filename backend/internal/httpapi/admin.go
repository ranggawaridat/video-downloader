package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"videodl/backend/internal/billing"
)

// requireAdmin: hanya user dengan role 'admin'.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := userFromContext(r.Context())
		if u == nil {
			writeError(w, http.StatusUnauthorized, "login_required", "Masuk sebagai admin")
			return
		}
		if u.Role != "admin" {
			writeError(w, http.StatusForbidden, "forbidden", "Butuh akses admin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- overview ----------

type adminStats struct {
	TotalUsers     int64 `json:"total_users"`
	NewUsers24h    int64 `json:"new_users_24h"`
	TotalDownloads int64 `json:"total_downloads"`
	Downloads24h   int64 `json:"downloads_24h"`
	Completed24h   int64 `json:"completed_24h"`
	FailedTotal    int64 `json:"failed_total"`
	QueueDepth     int64 `json:"queue_depth"`
	StorageBytes   int64 `json:"storage_bytes"`
	ActiveSubs     int64 `json:"active_subs"`
	MRRIDR         int64 `json:"mrr_idr"`
}

type providerCount struct {
	Provider string `json:"provider"`
	Count    int64  `json:"count"`
}

type dailyCount struct {
	Day   string `json:"day"`
	Count int64  `json:"count"`
}

type failureRow struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	Title        string    `json:"title"`
	ErrorCode    string    `json:"error_code"`
	ErrorMessage string    `json:"error_message"`
	Attempts     int       `json:"attempts"`
	CreatedAt    time.Time `json:"created_at"`
}

// overview: statistik utama untuk dashboard.
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	var s adminStats
	err := h.pool.QueryRow(r.Context(), `
		SELECT
			(SELECT count(*) FROM users),
			(SELECT count(*) FROM users WHERE created_at > now() - interval '24 hours'),
			(SELECT count(*) FROM downloads),
			(SELECT count(*) FROM downloads WHERE created_at > now() - interval '24 hours'),
			(SELECT count(*) FROM downloads WHERE status = 'completed' AND created_at > now() - interval '24 hours'),
			(SELECT count(*) FROM downloads WHERE status = 'failed'),
			(SELECT count(*) FROM downloads WHERE status IN ('queued','running')),
			(SELECT COALESCE(sum(file_size), 0) FROM downloads WHERE status = 'completed'),
			(SELECT count(*) FROM subscriptions WHERE status = 'active' AND period_end > now()),
			(SELECT COALESCE(sum(p.price_idr), 0)
			   FROM subscriptions s JOIN plans p ON p.id = s.plan_id
			  WHERE s.status = 'active' AND s.period_end > now())
	`).Scan(&s.TotalUsers, &s.NewUsers24h, &s.TotalDownloads, &s.Downloads24h,
		&s.Completed24h, &s.FailedTotal, &s.QueueDepth, &s.StorageBytes,
		&s.ActiveSubs, &s.MRRIDR)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	topRows, err := h.pool.Query(r.Context(), `
		SELECT provider, count(*) FROM downloads
		WHERE provider <> '' AND created_at > now() - interval '7 days'
		GROUP BY provider ORDER BY count(*) DESC LIMIT 5`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer topRows.Close()
	providers := []providerCount{}
	for topRows.Next() {
		var pc providerCount
		if err := topRows.Scan(&pc.Provider, &pc.Count); err == nil {
			providers = append(providers, pc)
		}
	}

	dailyRows, err := h.pool.Query(r.Context(), `
		SELECT to_char(date_trunc('day', created_at), 'YYYY-MM-DD') AS day, count(*)
		FROM downloads
		WHERE created_at > now() - interval '14 days'
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer dailyRows.Close()
	daily := []dailyCount{}
	for dailyRows.Next() {
		var dc dailyCount
		if err := dailyRows.Scan(&dc.Day, &dc.Count); err == nil {
			daily = append(daily, dc)
		}
	}

	failRows, err := h.pool.Query(r.Context(), `
		SELECT id::text, provider, title, error_code, error_message, attempts, created_at
		FROM downloads WHERE status = 'failed'
		ORDER BY created_at DESC LIMIT 10`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer failRows.Close()
	failures := []failureRow{}
	for failRows.Next() {
		var fr failureRow
		if err := failRows.Scan(&fr.ID, &fr.Provider, &fr.Title, &fr.ErrorCode,
			&fr.ErrorMessage, &fr.Attempts, &fr.CreatedAt); err == nil {
			failures = append(failures, fr)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"totals":          s,
		"top_providers":   providers,
		"daily":           daily,
		"recent_failures": failures,
	})
}

// ---------- users ----------

type adminUserRow struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	Plan      string    `json:"plan"`
	Downloads int64     `json:"downloads"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *Handler) adminListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	rows, err := h.pool.Query(r.Context(), `
		SELECT u.id::text, u.email, u.role,
		       COALESCE(sp.code, 'free') AS plan,
		       (SELECT count(*) FROM downloads d WHERE d.user_id = u.id),
		       u.created_at
		FROM users u
		LEFT JOIN LATERAL (
			SELECT p.code FROM subscriptions s
			JOIN plans p ON p.id = s.plan_id
			WHERE s.user_id = u.id AND s.status = 'active' AND s.period_end > now()
			ORDER BY s.period_end DESC LIMIT 1
		) sp ON true
		WHERE ($1 = '' OR u.email ILIKE '%' || $1 || '%')
		ORDER BY u.created_at DESC
		LIMIT 50`, q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer rows.Close()

	items := []adminUserRow{}
	for rows.Next() {
		var u adminUserRow
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.Plan, &u.Downloads, &u.CreatedAt); err == nil {
			items = append(items, u)
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// adminGrantPlan: aktivasi manual langganan untuk user (mengganti yang aktif).
func (h *Handler) adminGrantPlan(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		PlanCode string `json:"plan_code"`
		Days     int    `json:"days"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Days <= 0 || req.Days > 365 {
		req.Days = 30
	}
	pl, err := billing.PlanByCode(r.Context(), h.pool, req.PlanCode)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && pl == nil) {
		writeError(w, http.StatusBadRequest, "invalid_plan", "Paket tidak ditemukan")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	_ = billing.CancelActive(r.Context(), h.pool, id)
	if err := billing.ActivateManual(r.Context(), h.pool, id, pl.ID, req.Days); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "plan_code": pl.Code, "days": req.Days,
	})
}

// ---------- downloads ----------

type adminDownloadRow struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	ErrorCode    string    `json:"error_code"`
	ErrorMessage string    `json:"error_message"`
	Attempts     int       `json:"attempts"`
	UserEmail    string    `json:"user_email"`
	CreatedAt    time.Time `json:"created_at"`
}

func (h *Handler) adminListDownloads(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	rows, err := h.pool.Query(r.Context(), `
		SELECT d.id::text, d.provider, d.title, d.status, d.error_code, d.error_message,
		       d.attempts, COALESCE(u.email, ''), d.created_at
		FROM downloads d LEFT JOIN users u ON u.id = d.user_id
		WHERE ($1 = '' OR d.status = $1)
		ORDER BY d.created_at DESC
		LIMIT 50`, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer rows.Close()

	items := []adminDownloadRow{}
	for rows.Next() {
		var d adminDownloadRow
		if err := rows.Scan(&d.ID, &d.Provider, &d.Title, &d.Status, &d.ErrorCode,
			&d.ErrorMessage, &d.Attempts, &d.UserEmail, &d.CreatedAt); err == nil {
			items = append(items, d)
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// adminRetryDownload: requeue job failed/canceled (reset attempts).
func (h *Handler) adminRetryDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ct, err := h.pool.Exec(r.Context(), `
		UPDATE downloads
		SET status = 'queued', progress = 0, next_run_at = NULL,
		    attempts = 0, error_code = '', error_message = ''
		WHERE id = $1 AND status IN ('failed','canceled')`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if ct.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_retryable",
			"Job tidak ditemukan atau tidak bisa di-retry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "queued"})
}

// ---------- plans ----------

type adminPlanRow struct {
	billing.Plan
	Subscribers int64 `json:"subscribers"`
	Payments    int64 `json:"payments"`
}

// adminListPlans: semua paket (termasuk nonaktif) + jumlah pemakaian,
// supaya admin bisa mengaktifkan kembali atau melihat dampak penghapusan.
func (h *Handler) adminListPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := h.pool.Query(r.Context(), `
		SELECT p.id::text, p.code, p.name, p.daily_limit, p.max_height,
		       p.retention_hours, p.price_idr, p.active, p.sort_order,
		       (SELECT count(*) FROM subscriptions s
		         WHERE s.plan_id = p.id AND s.status = 'active' AND s.period_end > now()),
		       (SELECT count(*) FROM payments pm WHERE pm.plan_id = p.id)
		FROM plans p
		ORDER BY p.sort_order, p.price_idr, p.code`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	defer rows.Close()

	items := []adminPlanRow{}
	for rows.Next() {
		var it adminPlanRow
		if err := rows.Scan(&it.ID, &it.Code, &it.Name, &it.DailyLimit, &it.MaxHeight,
			&it.RetentionHours, &it.PriceIDR, &it.Active, &it.SortOrder,
			&it.Subscribers, &it.Payments); err == nil {
			items = append(items, it)
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// validPlanCode: slug huruf kecil/angka dengan - atau _.
func validPlanCode(code string) bool {
	if code == "" || len(code) > 32 {
		return false
	}
	for _, c := range code {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// adminCreatePlan: tambah paket baru dari panel.
func (h *Handler) adminCreatePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code           string `json:"code"`
		Name           string `json:"name"`
		DailyLimit     int    `json:"daily_limit"`
		MaxHeight      int    `json:"max_height"`
		RetentionHours int    `json:"retention_hours"`
		PriceIDR       int    `json:"price_idr"`
		SortOrder      int    `json:"sort_order"`
		Active         *bool  `json:"active"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	req.Code = strings.ToLower(strings.TrimSpace(req.Code))
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case !validPlanCode(req.Code):
		writeError(w, http.StatusBadRequest, "validation",
			"Kode paket harus huruf kecil/angka/'-'/'_' (maks 32 karakter)")
		return
	case req.Name == "":
		writeError(w, http.StatusBadRequest, "validation", "Nama paket wajib diisi")
		return
	case req.DailyLimit <= 0:
		writeError(w, http.StatusBadRequest, "validation", "Kuota harian harus > 0")
		return
	case req.MaxHeight <= 0:
		writeError(w, http.StatusBadRequest, "validation", "Resolusi maks harus > 0")
		return
	case req.RetentionHours <= 0:
		writeError(w, http.StatusBadRequest, "validation", "Retensi harus > 0")
		return
	case req.PriceIDR < 0:
		writeError(w, http.StatusBadRequest, "validation", "Harga tidak boleh negatif")
		return
	case req.SortOrder < 0:
		writeError(w, http.StatusBadRequest, "validation", "Urutan tidak boleh negatif")
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	// Paket gratis (harga 0) tidak boleh dibuat nonaktif dari awal —
	// tidak ada gunanya dan membingungkan halaman harga.
	if req.PriceIDR == 0 {
		active = true
	}

	pl, err := billing.CreatePlan(r.Context(), h.pool, billing.PlanInput{
		Code:           req.Code,
		Name:           req.Name,
		DailyLimit:     req.DailyLimit,
		MaxHeight:      req.MaxHeight,
		RetentionHours: req.RetentionHours,
		PriceIDR:       req.PriceIDR,
		SortOrder:      req.SortOrder,
		Active:         active,
	})
	if err != nil {
		h.log.Error("create plan failed", "code", req.Code, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if pl == nil {
		writeError(w, http.StatusConflict, "duplicate_code", "Kode paket sudah dipakai")
		return
	}
	writeJSON(w, http.StatusCreated, pl)
}

// adminDeletePlan: hapus paket; bila masih dipakai langganan/pembayaran,
// paket hanya dinonaktifkan agar riwayat tetap konsisten.
func (h *Handler) adminDeletePlan(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")

	// Ambil dulu: setelah hard delete datanya sudah tidak ada.
	pl, err := billing.PlanByCodeAny(r.Context(), h.pool, code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if pl == nil {
		writeError(w, http.StatusNotFound, "not_found", "Paket tidak ditemukan")
		return
	}

	deleted, err := billing.DeletePlan(r.Context(), h.pool, code)
	if errors.Is(err, billing.ErrPlanProtected) {
		writeError(w, http.StatusBadRequest, "plan_protected",
			"Paket free tidak bisa dihapus (dipakai sebagai paket bawaan)")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}

	status := "deleted"
	if !deleted {
		status = "deactivated"
		pl.Active = false // refleksikan keadaan terbaru
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status, "plan": pl,
	})
}

// adminUpdatePlan: ubah benefit plan langsung dari panel (tanpa SQL manual).
func (h *Handler) adminUpdatePlan(w http.ResponseWriter, r *http.Request) {
	code := chi.URLParam(r, "code")
	var req struct {
		Name           *string `json:"name"`
		DailyLimit     *int    `json:"daily_limit"`
		MaxHeight      *int    `json:"max_height"`
		RetentionHours *int    `json:"retention_hours"`
		PriceIDR       *int    `json:"price_idr"`
		SortOrder      *int    `json:"sort_order"`
		Active         *bool   `json:"active"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}

	intArg := func(p *int) any {
		if p == nil {
			return nil
		}
		return *p
	}
	strArg := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	if v := req.Name; v != nil && strings.TrimSpace(*v) == "" {
		writeError(w, http.StatusBadRequest, "validation", "Nama paket tidak boleh kosong")
		return
	}
	if v := req.DailyLimit; v != nil && *v <= 0 {
		writeError(w, http.StatusBadRequest, "validation", "daily_limit harus > 0")
		return
	}
	if v := req.MaxHeight; v != nil && *v <= 0 {
		writeError(w, http.StatusBadRequest, "validation", "max_height harus > 0")
		return
	}
	if v := req.RetentionHours; v != nil && *v <= 0 {
		writeError(w, http.StatusBadRequest, "validation", "retention_hours harus > 0")
		return
	}
	if v := req.PriceIDR; v != nil && *v < 0 {
		writeError(w, http.StatusBadRequest, "validation", "price_idr tidak boleh negatif")
		return
	}
	if v := req.SortOrder; v != nil && *v < 0 {
		writeError(w, http.StatusBadRequest, "validation", "sort_order tidak boleh negatif")
		return
	}
	// Paket bawaan wajib tetap aktif (fallback pengguna tanpa langganan).
	if code == "free" && req.Active != nil && !*req.Active {
		writeError(w, http.StatusBadRequest, "plan_protected",
			"Paket free harus tetap aktif")
		return
	}

	ct, err := h.pool.Exec(r.Context(), `
		UPDATE plans
		SET name = COALESCE($1, name),
		    daily_limit = COALESCE($2, daily_limit),
		    max_height = COALESCE($3, max_height),
		    retention_hours = COALESCE($4, retention_hours),
		    price_idr = COALESCE($5, price_idr),
		    sort_order = COALESCE($6, sort_order),
		    active = COALESCE($7, active)
		WHERE code = $8`,
		strArg(req.Name), intArg(req.DailyLimit), intArg(req.MaxHeight),
		intArg(req.RetentionHours), intArg(req.PriceIDR), intArg(req.SortOrder),
		req.Active, code)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	if ct.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "not_found", "Paket tidak ditemukan")
		return
	}

	pl, err := billing.PlanByCodeAny(r.Context(), h.pool, code)
	if err != nil || pl == nil {
		writeError(w, http.StatusInternalServerError, "internal", "Terjadi kesalahan")
		return
	}
	writeJSON(w, http.StatusOK, pl)
}
