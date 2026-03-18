// backend/telegram.go — Telegram Bot API storage backend.
//
// ZenithVault uses Telegram purely as a dumb byte store. The bot/chat pair
// receives ONLY encrypted binary blobs — it has no knowledge of filenames,
// keys, or file contents.
package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"time"
)

const (
	apiRoot        = "https://api.telegram.org"
	downloadBufSize = 2 * 1024 * 1024 // 2 MB streaming read buffer

	// Retry policy
	maxRetries       = 3
	retryBaseDelay   = 500 * time.Millisecond // doubles each attempt
	retryMaxDelay    = 16 * time.Second
	retryJitterFrac  = 4 // jitter = base/jitterFrac
)

// TelegramClient is an HTTP client for the Telegram Bot API.
type TelegramClient struct {
	token  string
	chatID string
	base   string
	http   *http.Client
}

// UploadResult holds the file_id and message_id after a successful upload.
type UploadResult struct {
	FileID    string
	MessageID int64
}

// NewTelegramClient creates a new client for the given bot token and chat ID.
func NewTelegramClient(token, chatID string) *TelegramClient {
	return &TelegramClient{
		token:  token,
		chatID: chatID,
		base:   fmt.Sprintf("%s/bot%s", apiRoot, token),
		http: &http.Client{
			// No global timeout; individual request timeouts are applied via
			// the transport and per-operation context deadlines.
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 90 * time.Second, // headers only, not body
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// ── Internal types ─────────────────────────────────────────────────────────────

type apiResponseFull struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// ── Core HTTP helpers ──────────────────────────────────────────────────────────

// doOnce executes req and decodes the Telegram response.
// Returns (result, retryable, error).
func (c *TelegramClient) doOnce(req *http.Request) (json.RawMessage, bool, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, isRetryableNetErr(err), fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, fmt.Errorf("read body: %w", err)
	}

	var ar apiResponseFull
	if err := json.Unmarshal(body, &ar); err != nil {
		// Not a valid JSON Telegram response — likely a 5xx HTML error page.
		return nil, resp.StatusCode >= 500, fmt.Errorf("parse response (HTTP %d): %w", resp.StatusCode, err)
	}

	// 429 Too Many Requests: sleep the server-requested duration, then signal retry.
	if resp.StatusCode == 429 || ar.ErrorCode == 429 {
		wait := time.Duration(ar.Parameters.RetryAfter+1) * time.Second
		if wait < time.Second {
			wait = 2 * time.Second
		}
		time.Sleep(wait)
		return nil, true, fmt.Errorf("rate limited (retry after %ds)", ar.Parameters.RetryAfter)
	}

	// 5xx server errors are transient — worth retrying.
	if resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("server error %d: %s", resp.StatusCode, ar.Description)
	}

	if !ar.OK {
		// 4xx client errors (e.g. bad token, file not found) are permanent.
		return nil, false, fmt.Errorf("Telegram API error: %s", ar.Description)
	}

	return ar.Result, false, nil
}

// withRetry calls makeReq and retries on transient errors with exponential backoff.
// The body is rebuilt on each attempt via makeReq so consumed readers aren't reused.
func (c *TelegramClient) withRetry(makeReq func() (*http.Request, error)) (json.RawMessage, error) {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * (1 << uint(attempt-1))
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
			jitter := time.Duration(rand.Int63n(int64(delay) / retryJitterFrac))
			time.Sleep(delay + jitter)
		}

		req, err := makeReq()
		if err != nil {
			return nil, err // request construction failure is never retried
		}

		result, retry, err := c.doOnce(req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("all %d attempts failed: %w", maxRetries+1, lastErr)
}

// getJSON performs a GET request to a Telegram Bot API method with retry.
func (c *TelegramClient) getJSON(method string, params map[string]string) (json.RawMessage, error) {
	return c.withRetry(func() (*http.Request, error) {
		url := fmt.Sprintf("%s/%s", c.base, method)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		if params != nil {
			q := req.URL.Query()
			for k, v := range params {
				q.Set(k, v)
			}
			req.URL.RawQuery = q.Encode()
		}
		return req, nil
	})
}

// postMultipart performs a multipart POST to a Telegram Bot API method with retry.
// buildBody is called on each attempt so the buffer is always fresh.
func (c *TelegramClient) postMultipart(method string, buildBody func() (*bytes.Buffer, string, error)) (json.RawMessage, error) {
	return c.withRetry(func() (*http.Request, error) {
		body, contentType, err := buildBody()
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s/%s", c.base, method)
		req, err := http.NewRequest("POST", url, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", contentType)
		return req, nil
	})
}

// ── Public API ────────────────────────────────────────────────────────────────

// UploadFile uploads an encrypted blob to the configured Telegram chat.
func (c *TelegramClient) UploadFile(displayName string, encryptedData []byte) (*UploadResult, error) {
	result, err := c.postMultipart("sendDocument", func() (*bytes.Buffer, string, error) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("chat_id", c.chatID)

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="document"; filename="%s"`, displayName))
		h.Set("Content-Type", "application/octet-stream")
		part, err := writer.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(encryptedData); err != nil {
			return nil, "", err
		}
		writer.Close()
		return &body, writer.FormDataContentType(), nil
	})
	if err != nil {
		return nil, err
	}

	var msg struct {
		MessageID int64 `json:"message_id"`
		Document  struct {
			FileID string `json:"file_id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(result, &msg); err != nil {
		return nil, fmt.Errorf("parse sendDocument response: %w", err)
	}
	return &UploadResult{FileID: msg.Document.FileID, MessageID: msg.MessageID}, nil
}

// VerifyChunk confirms that the uploaded chunk is accessible on Telegram's CDN
// by calling getFile and checking that a download path is returned.
// This is called immediately after upload to catch CDN propagation failures.
func (c *TelegramClient) VerifyChunk(fileID string) error {
	raw, err := c.getJSON("getFile", map[string]string{"file_id": fileID})
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	var info struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("verify: parse response: %w", err)
	}
	if info.FilePath == "" {
		return errors.New("verify: chunk not accessible (empty CDN path)")
	}
	return nil
}

// DownloadFile stream-downloads the encrypted blob for fileID into dst.
// Returns the total bytes written.
// dst must implement io.WriterTo or accept being written to from the beginning
// on each retry attempt (i.e. callers should pass a fresh *bytes.Buffer).
func (c *TelegramClient) DownloadFile(fileID string, dst io.Writer) (int64, error) {
	raw, err := c.getJSON("getFile", map[string]string{"file_id": fileID})
	if err != nil {
		if containsAny(err.Error(), "file is too big", "400") {
			return 0, fmt.Errorf(
				"chunk exceeds Telegram's 20 MB download limit — please delete and re-upload")
		}
		return 0, err
	}

	var fileInfo struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &fileInfo); err != nil {
		return 0, err
	}

	dlURL := fmt.Sprintf("%s/file/bot%s/%s", apiRoot, c.token, fileInfo.FilePath)
	buf := make([]byte, downloadBufSize)

	// CDN download: manual retry loop (body streaming can't go through withRetry).
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := retryBaseDelay * (1 << uint(attempt-1))
			if delay > retryMaxDelay {
				delay = retryMaxDelay
			}
			jitter := time.Duration(rand.Int63n(int64(delay) / retryJitterFrac))
			time.Sleep(delay + jitter)

			// Reset dst if it supports truncation so partial data from the
			// previous attempt doesn't corrupt the result.
			if resetter, ok := dst.(interface{ Reset() }); ok {
				resetter.Reset()
			}
		}

		resp, err := c.http.Get(dlURL)
		if err != nil {
			lastErr = err
			if isRetryableNetErr(err) {
				continue
			}
			return 0, err
		}
		if resp.StatusCode == 429 {
			resp.Body.Close()
			time.Sleep(2 * time.Second)
			lastErr = fmt.Errorf("CDN rate limited")
			continue
		}
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("CDN error HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return 0, fmt.Errorf("CDN HTTP %d", resp.StatusCode)
		}

		n, err := io.CopyBuffer(dst, resp.Body, buf)
		resp.Body.Close()
		if err == nil {
			return n, nil
		}
		lastErr = err
		if !isRetryableNetErr(err) {
			return 0, err
		}
	}
	return 0, fmt.Errorf("download failed after %d attempts: %w", maxRetries+1, lastErr)
}

// DeleteMessage removes a message (and its file attachment) from the chat.
// Returns true on success, false on error (non-fatal for callers).
func (c *TelegramClient) DeleteMessage(messageID int64) bool {
	_, err := c.postMultipart("deleteMessage", func() (*bytes.Buffer, string, error) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("chat_id", c.chatID)
		_ = writer.WriteField("message_id", fmt.Sprintf("%d", messageID))
		writer.Close()
		return &body, writer.FormDataContentType(), nil
	})
	return err == nil
}

// TestConnection validates the bot token. Returns the bot info on success.
func (c *TelegramClient) TestConnection() (map[string]interface{}, error) {
	raw, err := c.getJSON("getMe", nil)
	if err != nil {
		return nil, err
	}
	var info map[string]interface{}
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	return info, nil
}

// ── Helpers ────────────────────────────────────────────────────────────────────

// isRetryableNetErr returns true for transient network errors worth retrying.
func isRetryableNetErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"connection reset", "connection refused", "i/o timeout",
		"EOF", "broken pipe", "no route to host", "temporary failure",
		"context deadline exceeded")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
