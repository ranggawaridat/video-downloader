// Package telegram: client Telegram Bot API + logic bot downloader.
// Token dari @BotFather via env TELEGRAM_BOT_TOKEN; kosong = bot off.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

// Client wrapper minimal Bot API.
type Client struct {
	token string
	http  *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token: token,
		// Tanpa timeout global: upload file bisa lama; tiap request
		// memakai context sendiri.
		http: &http.Client{},
	}
}

func (c *Client) Configured() bool { return c != nil && c.token != "" }

func (c *Client) callJSON(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		apiBase+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return err
	}
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("telegram: respons tidak valid: %s", truncate(string(body), 200))
	}
	if !env.OK {
		return &APIError{Code: env.ErrorCode, Description: env.Description}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return err
		}
	}
	return nil
}

// APIError error dari Telegram (mis. 429 Too Many Requests).
type APIError struct {
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram api %d: %s", e.Code, e.Description)
}

func (e *APIError) RetryAfter() time.Duration {
	if e.Code == 429 {
		return 5 * time.Second // fallback sederhana
	}
	return 0
}

// ---------- update ----------

type Update struct {
	ID       int64 `json:"update_id"`
	Message  *Msg  `json:"message"`
	Callback *Cb   `json:"callback_query"`
}

type Msg struct {
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private|group|...
}

type Cb struct {
	ID      string `json:"id"`
	From    User   `json:"from"`
	Message *Msg   `json:"message"`
	Data    string `json:"data"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// GetUpdates long polling; timeout detik di server Telegram.
func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var out struct {
		Result []Update `json:"result"`
	}
	err := c.callJSON(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         30,
		"allowed_updates": []string{"message", "callback_query"},
	}, &out)
	if err != nil {
		return nil, err
	}
	return out.Result, nil
}

// ---------- keyboard ----------

type InlineButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type Keyboard struct {
	Rows [][]InlineButton `json:"inline_keyboard"`
}

func (k *Keyboard) marshalParam() map[string]any {
	b, _ := json.Marshal(k)
	return map[string]any{"inline_keyboard": json.RawMessage(b)}
}

// ---------- aksi ----------

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, kb *Keyboard) error {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if kb != nil {
		payload["reply_markup"] = kb.marshalParam()
	}
	return c.callJSON(ctx, "sendMessage", payload, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, cbID, text string) error {
	return c.callJSON(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": cbID,
		"text":              text,
	}, nil)
}

// SendDocument mengunggah file dari disk (maks 50MB di Bot API).
func (c *Client) SendDocument(ctx context.Context, chatID int64, path, filename string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	go func() {
		part, err := w.CreateFormFile("document", filename)
		if err == nil {
			_, err = io.Copy(part, f)
		}
		w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
		w.Close()
		if err != nil {
			pw.CloseWithError(err)
		} else {
			pw.Close()
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		apiBase+c.token+"/sendDocument", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.do(req, nil)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
