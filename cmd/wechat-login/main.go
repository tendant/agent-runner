// wechat-login is a CLI tool that performs the WeChat iLink QR-code login flow
// and prints the resulting WECHAT_TOKEN so you can add it to your .env file.
//
// Usage:
//
//	go run ./cmd/wechat-login
//	go run ./cmd/wechat-login -base-url https://ilinkai.weixin.qq.com
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/agent-runner/agent-runner/internal/wechat"
)

func main() {
	baseURL := flag.String("base-url", "https://ilinkai.weixin.qq.com", "iLink API base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "total time to wait for QR scan")
	flag.Parse()

	client := wechat.NewClient(*baseURL, "", "") // no token needed for login flow

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Step 1: fetch QR code
	fmt.Println("Fetching WeChat QR code...")
	qrResp, err := client.GetQRCode(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if qrResp.QRCodeImgContent == "" {
		fmt.Fprintf(os.Stderr, "error: empty qrcode_img_content in response (ret=%d errmsg=%s)\n",
			qrResp.Ret, qrResp.ErrMsg)
		os.Exit(1)
	}

	// Step 2: display QR code in terminal
	fmt.Println("\nScan the QR code below with WeChat:")
	fmt.Println()
	qrterminal.GenerateWithConfig(qrResp.QRCodeImgContent, qrterminal.Config{
		Level:          qrterminal.L,
		Writer:         os.Stdout,
		BlackChar:      qrterminal.BLACK,
		WhiteChar:      qrterminal.WHITE,
		QuietZone:      1,
		BlackWhiteChar: qrterminal.BLACK_WHITE,
	})
	fmt.Println()
	fmt.Printf("(URL: %s)\n\n", qrResp.QRCodeImgContent)

	// Step 3: poll until confirmed
	fmt.Println("Waiting for scan...")
	deadline := time.Now().Add(*timeout)
	maxRefreshes := 3
	refreshes := 0
	qrCode := qrResp.QRCode

	for {
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "error: timed out waiting for QR scan")
			os.Exit(1)
		}

		pollCtx, pollCancel := context.WithTimeout(ctx, 37*time.Second)
		statusResp, err := client.PollQRCodeStatus(pollCtx, qrCode)
		pollCancel()

		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "error: cancelled")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "poll error: %v — retrying...\n", err)
			time.Sleep(2 * time.Second)
			continue
		}

		switch statusResp.Status {
		case "wait":
			fmt.Print(".")

		case "scaned":
			fmt.Println("\nQR code scanned — confirm in WeChat...")

		case "expired":
			refreshes++
			if refreshes > maxRefreshes {
				fmt.Fprintln(os.Stderr, "\nerror: QR code expired too many times")
				os.Exit(1)
			}
			fmt.Printf("\nQR code expired — refreshing (%d/%d)...\n", refreshes, maxRefreshes)

			newQR, err := client.GetQRCode(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error refreshing QR code: %v\n", err)
				os.Exit(1)
			}
			qrCode = newQR.QRCode
			fmt.Println()
			qrterminal.GenerateWithConfig(newQR.QRCodeImgContent, qrterminal.Config{
				Level:          qrterminal.L,
				Writer:         os.Stdout,
				BlackChar:      qrterminal.BLACK,
				WhiteChar:      qrterminal.WHITE,
				QuietZone:      1,
				BlackWhiteChar: qrterminal.BLACK_WHITE,
			})
			fmt.Printf("(URL: %s)\n\n", newQR.QRCodeImgContent)

		case "confirmed":
			fmt.Println("\nLogin confirmed!")
			if statusResp.BotToken == "" {
				fmt.Fprintln(os.Stderr, "error: confirmed but no bot_token in response")
				os.Exit(1)
			}
			updates := map[string]string{
				"WECHAT_TOKEN": statusResp.BotToken,
			}
			if statusResp.BaseURL != "" && statusResp.BaseURL != *baseURL {
				updates["WECHAT_BASE_URL"] = statusResp.BaseURL
			}
			if err := upsertEnvFile(".env", updates); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write .env: %v\n", err)
				fmt.Printf("WECHAT_TOKEN=%s\n", statusResp.BotToken)
				if statusResp.BaseURL != "" && statusResp.BaseURL != *baseURL {
					fmt.Printf("WECHAT_BASE_URL=%s\n", statusResp.BaseURL)
				}
			} else {
				fmt.Println("Saved to .env — restart agent-runner to apply.")
			}
			return

		default:
			fmt.Printf("\nunknown status: %s\n", statusResp.Status)
		}
	}
}

// upsertEnvFile updates or appends key=value pairs in a .env file.
// Creates the file if it does not exist.
func upsertEnvFile(path string, updates map[string]string) error {
	var lines []string
	remaining := make(map[string]string, len(updates))
	for k, v := range updates {
		remaining[k] = v
	}

	f, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			key, _, found := strings.Cut(line, "=")
			if found {
				key = strings.TrimSpace(key)
				if val, ok := remaining[key]; ok {
					line = key + "=" + val
					delete(remaining, key)
				}
			}
			lines = append(lines, line)
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
	}

	// Append keys that were not already present.
	for k, v := range remaining {
		lines = append(lines, k+"="+v)
	}

	out := strings.Join(lines, "\n")
	if len(lines) > 0 {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0600)
}
