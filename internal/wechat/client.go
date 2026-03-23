package wechat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const defaultBaseURL = "https://ilinkai.weixin.qq.com"

// Client is an HTTP client for the Tencent iLink bot API.
type Client struct {
	baseURL    string
	token      string
	stateDir   string // directory for persisted state (sync buf cursor)
	httpClient *http.Client

	timeoutMu   sync.Mutex
	pollTimeout time.Duration
}

// NewClient creates an iLink API client. stateDir is the directory used to
// persist the getupdates cursor across restarts; if empty, the cursor is
// not persisted.
func NewClient(baseURL, token, stateDir string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:  baseURL,
		token:    token,
		stateDir: stateDir,
		httpClient: &http.Client{
			Timeout: 0, // callers set per-request timeouts via context
		},
	}
}

// GetUpdates long-polls for inbound messages. buf is the sync cursor from the
// previous call (empty string on first call). The server holds the connection
// for up to ~35 seconds before returning an empty response.
func (c *Client) GetUpdates(ctx context.Context, buf string) (*GetUpdatesResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 37*time.Second)
	defer cancel()

	slog.Debug("wechat: getupdates poll", "buf_len", len(buf))

	body, err := c.do(ctx, http.MethodPost, "ilink/bot/getupdates", GetUpdatesReq{
		GetUpdatesBuf: buf,
		BaseInfo:      buildBaseInfo(),
	})
	if err != nil {
		return nil, err
	}
	var resp GetUpdatesResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("wechat: parse getupdates response: %w", err)
	}
	slog.Debug("wechat: getupdates response",
		"ret", resp.Ret,
		"errcode", resp.ErrCode,
		"errmsg", resp.ErrMsg,
		"msgs", len(resp.Msgs),
		"new_buf_len", len(resp.GetUpdatesBuf),
	)
	if (resp.Ret != 0 || resp.ErrCode != 0) && resp.ErrMsg != "" {
		slog.Warn("wechat: getupdates api error",
			"ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg)
	}
	return &resp, nil
}

// SendMessage sends a plain-text message to a WeChat user.
// contextToken must be the token received from the most recent inbound message
// from that user; it is required for the server to route the reply correctly.
func (c *Client) SendMessage(ctx context.Context, toUserID, text, contextToken string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	slog.Info("wechat: sendmessage",
		"to_user_id", toUserID,
		"text_len", len(text),
		"has_context_token", contextToken != "",
	)

	req := SendMessageReq{
		Msg: WeixinMessage{
			FromUserID:   "",
			ToUserID:     toUserID,
			ClientID:     uuid.New().String(),
			MessageType:  MessageTypeBot,
			MessageState: MessageStateFinish,
			ContextToken: contextToken,
			ItemList: []MessageItem{
				{Type: MessageItemTypeText, TextItem: &TextItem{Text: text}},
			},
		},
		BaseInfo: buildBaseInfo(),
	}
	body, err := c.do(ctx, http.MethodPost, "ilink/bot/sendmessage", req)
	if err != nil {
		slog.Warn("wechat: sendmessage http error", "to_user_id", toUserID, "error", err)
		return err
	}

	// Server may return HTTP 200 with a non-zero ret/errcode on failure.
	var resp SendMessageResp
	if jsonErr := json.Unmarshal(body, &resp); jsonErr == nil {
		if resp.Ret != 0 || resp.ErrCode != 0 {
			err := fmt.Errorf("ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
			slog.Error("wechat: sendmessage api error", "to_user_id", toUserID, "error", err)
			return err
		}
	}
	slog.Info("wechat: sendmessage ok", "to_user_id", toUserID, "body", string(body))
	return nil
}

// GetQRCode fetches a QR code for interactive login.
func (c *Client) GetQRCode(ctx context.Context) (*GetQRCodeResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	body, err := c.do(ctx, http.MethodGet, "ilink/bot/get_bot_qrcode?bot_type=3", nil)
	if err != nil {
		return nil, err
	}
	var resp GetQRCodeResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("wechat: parse qrcode response: %w", err)
	}
	return &resp, nil
}

// PollQRCodeStatus polls until the user has scanned and confirmed the QR code.
// The server holds the connection for up to ~35 seconds per poll.
func (c *Client) PollQRCodeStatus(ctx context.Context, qrcode string) (*GetQRCodeStatusResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 37*time.Second)
	defer cancel()

	body, err := c.do(ctx, http.MethodGet, "ilink/bot/get_qrcode_status?qrcode="+qrcode, nil)
	if err != nil {
		return nil, err
	}
	var resp GetQRCodeStatusResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("wechat: parse qrcode status response: %w", err)
	}
	return &resp, nil
}

// do executes a request to the iLink API. Auth headers are added only when
// a token is configured (QR login endpoints are unauthenticated).
func (c *Client) do(ctx context.Context, method, path string, reqBody any) ([]byte, error) {
	var rawBody []byte
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("wechat: marshal request: %w", err)
		}
		rawBody = b
		bodyReader = bytes.NewReader(b)
	}

	url := c.baseURL + "/" + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("wechat: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		// QR login endpoints don't need a bearer token; add a version hint.
		req.Header.Set("iLink-App-ClientVersion", "1")
	}

	slog.Debug("wechat: http request", "method", method, "url", url, "body", string(rawBody))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		slog.Warn("wechat: http error", "method", method, "url", url, "error", err)
		return nil, fmt.Errorf("wechat: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wechat: read response body: %w", err)
	}

	slog.Debug("wechat: http response", "method", method, "url", url, "status", resp.StatusCode, "body", string(body))

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wechat: %s %s: HTTP %d: %s", method, path, resp.StatusCode, body)
	}
	return body, nil
}

func (c *Client) syncBufPath() string {
	return filepath.Join(c.stateDir, "wechat-sync-buf.txt")
}

// loadSyncBuf restores the last-known get_updates_buf cursor from disk.
// Returns "" if not found (first run) or if stateDir is not configured.
func (c *Client) loadSyncBuf() string {
	if c.stateDir == "" {
		return ""
	}
	data, err := os.ReadFile(c.syncBufPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// saveSyncBuf persists the get_updates_buf cursor to disk so it survives restarts.
func (c *Client) saveSyncBuf(buf string) {
	if c.stateDir == "" {
		return
	}
	if err := os.WriteFile(c.syncBufPath(), []byte(buf), 0600); err != nil {
		slog.Warn("wechat: failed to persist sync buf", "error", err)
	}
}

// setPollTimeout updates the suggested poll timeout from the server.
func (c *Client) setPollTimeout(d time.Duration) {
	c.timeoutMu.Lock()
	c.pollTimeout = d
	c.timeoutMu.Unlock()
}

// randomUIN returns X-WECHAT-UIN: the decimal string of a random uint32,
// base64-encoded. This matches the TypeScript implementation:
// Buffer.from(String(uint32), "utf-8").toString("base64")
func randomUIN() string {
	decimal := strconv.FormatUint(uint64(rand.Uint32()), 10)
	return base64.StdEncoding.EncodeToString([]byte(decimal))
}
