// Package midtrans: client Snap API, Core API (status transaksi), dan
// verifikasi signature webhook.
// Dokumentasi: https://docs.midtrans.com/docs/snap-api
package midtrans

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	ServerKey string
	// BaseURL host Snap API (app.*.midtrans.com).
	BaseURL string
	// APIBase host Core API (api.*.midtrans.com) — endpoint status transaksi.
	APIBase string
	HTTP    *http.Client
}

func New(serverKey string, production bool) *Client {
	snap := "https://app.sandbox.midtrans.com"
	core := "https://api.sandbox.midtrans.com"
	if production {
		snap = "https://app.midtrans.com"
		core = "https://api.midtrans.com"
	}
	return &Client{
		ServerKey: serverKey,
		BaseURL:   snap,
		APIBase:   core,
		HTTP:      &http.Client{Timeout: 20 * time.Second},
	}
}

func (c *Client) Configured() bool { return c != nil && c.ServerKey != "" }

// SnapTx parameter transaksi Snap.
type SnapTx struct {
	OrderID       string // maks 50 karakter
	GrossAmount   int    // IDR
	CustomerEmail string
	ItemID        string
	ItemName      string
	FinishURL     string // halaman yang dibuka setelah pembayaran selesai
	UnfinishURL   string // pembayaran belum selesai (opsional)
	ErrorURL      string // pembayaran gagal (opsional)
}

// CreateSnapTransaction membuat transaksi Snap, mengembalikan redirect_url
// yang dibuka pengguna untuk memilih metode bayar (QRIS, GoPay, VA, kartu).
func (c *Client) CreateSnapTransaction(ctx context.Context, t SnapTx) (redirectURL, token string, err error) {
	body := map[string]any{
		"transaction_details": map[string]any{
			"order_id":     t.OrderID,
			"gross_amount": t.GrossAmount,
		},
		"item_details": []map[string]any{{
			"id":       t.ItemID,
			"price":    t.GrossAmount,
			"quantity": 1,
			"name":     t.ItemName,
		}},
	}
	if t.CustomerEmail != "" {
		body["customer_details"] = map[string]any{"email": t.CustomerEmail}
	}
	// Callback: setelah pembayaran selesai/batal/gagal, Midtrans mengarahkan
	// browser kembali ke halaman kita (bukan layar "halaman Snap" buntu).
	if cb := callbackURLs(t); len(cb) > 0 {
		body["callbacks"] = cb
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/snap/v1/transactions", bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.ServerKey, "")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("midtrans request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("midtrans error %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		Token       string `json:"token"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", err
	}
	return out.RedirectURL, out.Token, nil
}

func callbackURLs(t SnapTx) map[string]any {
	cb := map[string]any{}
	if t.FinishURL != "" {
		cb["finish"] = t.FinishURL
	}
	if t.UnfinishURL != "" {
		cb["unfinish"] = t.UnfinishURL
	}
	if t.ErrorURL != "" {
		cb["error"] = t.ErrorURL
	}
	return cb
}

// TxStatus hasil GET Core API /v2/{order_id}/status.
type TxStatus struct {
	OrderID           string `json:"order_id"`
	StatusCode        string `json:"status_code"`
	GrossAmount       string `json:"gross_amount"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	TransactionID     string `json:"transaction_id"`
	PaymentType       string `json:"payment_type"`
}

// GetTransactionStatus menanyakan status transaksi langsung ke server
// Midtrans (terautentikasi server key). Dipakai sebagai sumber kebenaran
// saat notifikasi tidak bisa diverifikasi dan untuk rekonsiliasi.
func (c *Client) GetTransactionStatus(ctx context.Context, orderID string) (*TxStatus, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("midtrans: server key kosong")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.APIBase+"/v2/"+url.PathEscape(orderID)+"/status", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.ServerKey, "")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("midtrans status request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("midtrans status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out TxStatus
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Signature menghitung signature_key yang diharapkan:
// sha512(order_id + status_code + gross_amount + server_key).
// Diekspor untuk keperluan diagnostik/log.
func (c *Client) Signature(orderID, statusCode, grossAmount string) string {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + c.ServerKey))
	return hex.EncodeToString(sum[:])
}

// VerifySignature memverifikasi signature_key webhook.
func (c *Client) VerifySignature(orderID, statusCode, grossAmount, signature string) bool {
	want := c.Signature(orderID, statusCode, grossAmount)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(want)), []byte(strings.ToLower(signature))) == 1
}
