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
	"net/url"
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
	stateDir   string // directory for persisted state (sync buf cursor)
	httpClient *http.Client

	mu          sync.RWMutex
	token       string
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

// SetToken updates the bearer token and optionally the base URL at runtime.
// Safe to call concurrently with in-flight requests.
func (c *Client) SetToken(token, baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	if baseURL != "" {
		c.baseURL = baseURL
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

// GetUploadUrl requests CDN upload credentials for a media file.
func (c *Client) GetUploadUrl(ctx context.Context, req GetUploadUrlReq) (*GetUploadUrlResp, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req.BaseInfo = buildBaseInfo()
	body, err := c.do(ctx, http.MethodPost, "ilink/bot/getuploadurl", req)
	if err != nil {
		return nil, err
	}
	var resp GetUploadUrlResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("wechat: parse getuploadurl response: %w", err)
	}
	if resp.Ret != 0 || resp.ErrCode != 0 {
		return nil, fmt.Errorf("wechat: getuploadurl error ret=%d errcode=%d errmsg=%s", resp.Ret, resp.ErrCode, resp.ErrMsg)
	}
	return &resp, nil
}

// UploadToCDN POSTs encrypted bytes to the CDN and returns the download
// encrypted_query_param from the x-encrypted-param response header.
func (c *Client) UploadToCDN(ctx context.Context, cdnBaseURL, uploadParam, filekey string, data []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cdnURL := cdnBaseURL + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) +
		"&filekey=" + url.QueryEscape(filekey)
	slog.Debug("wechat: cdn upload", "url_prefix", cdnURL[:min(len(cdnURL), 80)], "bytes", len(data))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cdnURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("wechat: cdn upload build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("wechat: cdn upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg := resp.Header.Get("x-error-message")
		return "", fmt.Errorf("wechat: cdn upload HTTP %d: %s", resp.StatusCode, msg)
	}
	downloadParam := resp.Header.Get("x-encrypted-param")
	if downloadParam == "" {
		return "", fmt.Errorf("wechat: cdn upload response missing x-encrypted-param header")
	}
	slog.Debug("wechat: cdn upload ok", "download_param_len", len(downloadParam))
	return downloadParam, nil
}

// SendImage sends an image message to a WeChat user.
// downloadParam and aeskeyHex come from a prior UploadToCDN call.
func (c *Client) SendImage(ctx context.Context, toUserID, downloadParam, aeskeyHex, contextToken string, cipherSize int) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	slog.Info("wechat: sendimage", "to_user_id", toUserID, "has_context_token", contextToken != "")

	req := SendMessageReq{
		Msg: WeixinMessage{
			FromUserID:   "",
			ToUserID:     toUserID,
			ClientID:     uuid.New().String(),
			MessageType:  MessageTypeBot,
			MessageState: MessageStateFinish,
			ContextToken: contextToken,
			ItemList: []MessageItem{
				{
					Type: MessageItemTypeImage,
					ImageItem: &ImageItem{
						Media: &CDNMedia{
							EncryptQueryParam: downloadParam,
							// aes_key wire format: base64 of the hex string (not raw bytes)
							AESKey:      base64.StdEncoding.EncodeToString([]byte(aeskeyHex)),
							EncryptType: 1,
						},
						// mid_size is the ciphertext size in bytes
					},
				},
			},
		},
		BaseInfo: buildBaseInfo(),
	}
	// Store ciphertext size in mid_size (matches what the TypeScript sends)
	req.Msg.ItemList[0].ImageItem.MidSize = cipherSize

	body, err := c.do(ctx, http.MethodPost, "ilink/bot/sendmessage", req)
	if err != nil {
		return err
	}
	var sendResp SendMessageResp
	if jsonErr := json.Unmarshal(body, &sendResp); jsonErr == nil {
		if sendResp.Ret != 0 || sendResp.ErrCode != 0 {
			return fmt.Errorf("wechat: sendimage api error ret=%d errcode=%d errmsg=%s", sendResp.Ret, sendResp.ErrCode, sendResp.ErrMsg)
		}
	}
	slog.Info("wechat: sendimage ok", "to_user_id", toUserID)
	return nil
}

// SendFile sends a file message to a WeChat user.
// downloadParam, aeskeyHex, md5Hex and cipherSize come from a prior UploadToCDN call.
func (c *Client) SendFile(ctx context.Context, toUserID, fileName, downloadParam, aeskeyHex, md5Hex, contextToken string, cipherSize int) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	slog.Info("wechat: sendfile", "to_user_id", toUserID, "file_name", fileName)

	req := SendMessageReq{
		Msg: WeixinMessage{
			FromUserID:   "",
			ToUserID:     toUserID,
			ClientID:     uuid.New().String(),
			MessageType:  MessageTypeBot,
			MessageState: MessageStateFinish,
			ContextToken: contextToken,
			ItemList: []MessageItem{
				{
					Type: MessageItemTypeFile,
					FileItem: &FileItem{
						FileName: fileName,
						MD5:      md5Hex,
						Media: &CDNMedia{
							EncryptQueryParam: downloadParam,
							AESKey:            base64.StdEncoding.EncodeToString([]byte(aeskeyHex)),
							EncryptType:       1,
						},
					},
				},
			},
		},
		BaseInfo: buildBaseInfo(),
	}
	req.Msg.ItemList[0].FileItem.Len = strconv.Itoa(cipherSize)

	body, err := c.do(ctx, http.MethodPost, "ilink/bot/sendmessage", req)
	if err != nil {
		return err
	}
	var sendResp SendMessageResp
	if jsonErr := json.Unmarshal(body, &sendResp); jsonErr == nil {
		if sendResp.Ret != 0 || sendResp.ErrCode != 0 {
			return fmt.Errorf("wechat: sendfile api error ret=%d errcode=%d errmsg=%s", sendResp.Ret, sendResp.ErrCode, sendResp.ErrMsg)
		}
	}
	slog.Info("wechat: sendfile ok", "to_user_id", toUserID, "file_name", fileName)
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

	c.mu.RLock()
	baseURL := c.baseURL
	token := c.token
	c.mu.RUnlock()

	url := baseURL + "/" + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("wechat: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("X-WECHAT-UIN", randomUIN())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		// QR login endpoints don't need a bearer token; add a version hint.
		req.Header.Set("iLink-App-ClientVersion", "1")
	}

	slog.Debug("wechat: http request", "method", method, "url", url, "body", string(rawBody))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			slog.Debug("wechat: http request cancelled", "method", method, "url", url)
		} else {
			slog.Warn("wechat: http error", "method", method, "url", url, "error", err)
		}
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
	c.mu.Lock()
	c.pollTimeout = d
	c.mu.Unlock()
}

// randomUIN returns X-WECHAT-UIN: the decimal string of a random uint32,
// base64-encoded. This matches the TypeScript implementation:
// Buffer.from(String(uint32), "utf-8").toString("base64")
func randomUIN() string {
	decimal := strconv.FormatUint(uint64(rand.Uint32()), 10)
	return base64.StdEncoding.EncodeToString([]byte(decimal))
}
