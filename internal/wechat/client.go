package wechat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

const defaultBaseURL = "https://ilinkai.weixin.qq.com"

// Client is an HTTP client for the Tencent iLink bot API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates an iLink API client.
func NewClient(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
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
	return &resp, nil
}

// SendMessage sends a plain-text message to a WeChat user.
// contextToken must be the token received from the most recent inbound message
// from that user; it is required for the server to route the reply correctly.
func (c *Client) SendMessage(ctx context.Context, toUserID, text, contextToken string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req := SendMessageReq{
		Msg: WeixinMessage{
			ToUserID:     toUserID,
			ContextToken: contextToken,
			ItemList: []MessageItem{
				{Type: MessageTypeText, TextItem: &TextItem{Text: text}},
			},
		},
		BaseInfo: buildBaseInfo(),
	}
	_, err := c.do(ctx, http.MethodPost, "ilink/bot/sendmessage", req)
	return err
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
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("wechat: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+path, bodyReader)
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wechat: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wechat: read response body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("wechat: %s %s: HTTP %d: %s", method, path, resp.StatusCode, body)
	}
	return body, nil
}

// randomUIN returns X-WECHAT-UIN: the decimal string of a random uint32,
// base64-encoded. This matches the TypeScript implementation:
// Buffer.from(String(uint32), "utf-8").toString("base64")
func randomUIN() string {
	decimal := strconv.FormatUint(uint64(rand.Uint32()), 10)
	return base64.StdEncoding.EncodeToString([]byte(decimal))
}
