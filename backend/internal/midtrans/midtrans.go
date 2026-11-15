// Package midtrans: client Snap API + verifikasi signature webhook.
// Dokumentasi: https://docs.midtrans.com/docs/snap-api
package midtrans

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	ServerKey string
	BaseURL   string
	HTTP      *http.Client
}

func New(serverKey string, production bool) *Client {
	base := "https://app.sandbox.midtrans.com"
	if production {
		base = "https://app.midtrans.com"
	}
	return &Client{
		ServerKey: serverKey,
		BaseURL:   base,
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

// VerifySignature memverifikasi signature_key webhook:
// sha512(order_id + status_code + gross_amount + server_key).
func (c *Client) VerifySignature(orderID, statusCode, grossAmount, signature string) bool {
	sum := sha512.Sum512([]byte(orderID + statusCode + grossAmount + c.ServerKey))
	return strings.EqualFold(hex.EncodeToString(sum[:]), signature)
}
