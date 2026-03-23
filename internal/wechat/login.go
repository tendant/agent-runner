package wechat

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	qrMaxRefreshes      = 3
	qrPollTimeout       = 37 * time.Second
	qrLoginTotalTimeout = 5 * time.Minute
)

// LoginResult holds the credentials returned on a successful QR login.
type LoginResult struct {
	Token   string
	BaseURL string // non-empty only when the server returns a region-specific URL
}

// RunLoginFlow performs the iLink QR login sequence.
//
// sendMessage is called to relay status updates to the user.
// sendQRContent is called with the raw QR code content (a WeChat-native URL)
// whenever a new QR code needs to be sent to the user. The caller is responsible
// for formatting/uploading the QR code as appropriate (e.g. as a PNG image or
// as tappable link text).
//
// The context deadline governs the total wait time; callers should set a
// timeout of at least qrLoginTotalTimeout (5 minutes).
func RunLoginFlow(ctx context.Context, baseURL string, sendMessage func(string), sendQRContent func(ctx context.Context, qrContent string)) (*LoginResult, error) {
	client := NewClient(baseURL, "", "") // unauthenticated — login endpoints don't need a token

	ctx, cancel := context.WithTimeout(ctx, qrLoginTotalTimeout)
	defer cancel()

	qrResp, err := client.GetQRCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("get qr code: %w", err)
	}
	if qrResp.QRCodeImgContent == "" {
		return nil, fmt.Errorf("server returned empty qr code content (ret=%d errmsg=%s)", qrResp.Ret, qrResp.ErrMsg)
	}

	sendQRContent(ctx, qrResp.QRCodeImgContent)

	qrCode := qrResp.QRCode
	refreshes := 0

	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("login timed out waiting for QR scan")
		}

		pollCtx, pollCancel := context.WithTimeout(ctx, qrPollTimeout)
		statusResp, err := client.PollQRCodeStatus(pollCtx, qrCode)
		pollCancel()

		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("login cancelled")
			}
			slog.Warn("wechat: login poll error, retrying", "error", err)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("login cancelled")
			case <-time.After(2 * time.Second):
			}
			continue
		}

		switch statusResp.Status {
		case "wait":
			// still waiting — next poll

		case "scaned":
			sendMessage("QR code scanned — please confirm in WeChat...")

		case "expired":
			refreshes++
			if refreshes > qrMaxRefreshes {
				return nil, fmt.Errorf("QR code expired %d times, giving up", refreshes)
			}
			sendMessage(fmt.Sprintf("QR code expired, refreshing (%d/%d)...", refreshes, qrMaxRefreshes))
			newQR, err := client.GetQRCode(ctx)
			if err != nil {
				return nil, fmt.Errorf("refresh qr code: %w", err)
			}
			qrCode = newQR.QRCode
			sendQRContent(ctx, newQR.QRCodeImgContent)

		case "confirmed":
			if statusResp.BotToken == "" {
				return nil, fmt.Errorf("login confirmed but server returned no bot_token")
			}
			slog.Info("wechat: login confirmed", "has_base_url", statusResp.BaseURL != "")
			return &LoginResult{
				Token:   statusResp.BotToken,
				BaseURL: statusResp.BaseURL,
			}, nil

		default:
			slog.Warn("wechat: unknown qr status", "status", statusResp.Status)
		}
	}
}

// sendQRText sends the QR code content to the user as a tappable link (text fallback).
func sendQRText(sendMessage func(string), qrContent string) {
	sendMessage("Tap the link below in WeChat to authorize the bot login:\n\n" +
		qrContent +
		"\n\n(If the link does not open automatically, copy and paste it into WeChat's built-in browser.)")
}
