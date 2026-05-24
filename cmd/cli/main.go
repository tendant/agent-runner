package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/term"
)

func main() {
	envFile := flag.String("env", ".env", "env file to load (e.g. .env.video)")
	flag.Parse()

	_ = godotenv.Load(*envFile)

	bind := os.Getenv("API_BIND")
	if bind == "" {
		bind = "127.0.0.1:8080"
	}
	baseURL := "http://" + bind
	apiKey := os.Getenv("API_KEY")

	if checkServer(baseURL, apiKey) {
		fmt.Printf("agent-cli connected to %s (env: %s)\n", baseURL, *envFile)
	} else {
		fmt.Printf("agent-cli targeting %s (env: %s) — server not reachable\n", baseURL, *envFile)
	}
	repl(baseURL, apiKey)
}

// checkServer pings /health and returns true if the server is up.
func checkServer(baseURL, apiKey string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := apiRequest(ctx, http.MethodGet, baseURL+"/health", apiKey, nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func repl(baseURL, apiKey string) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		replPipe(baseURL, apiKey)
		return
	}

	state, err := term.MakeRaw(fd)
	if err != nil {
		replPipe(baseURL, apiKey)
		return
	}
	defer term.Restore(fd, state)
	fmt.Print("\x1b[?2004h")
	defer fmt.Print("\x1b[?2004l")

	in := bufio.NewReader(os.Stdin)

	for {
		msg, ok := readMessage(in)
		if !ok {
			fmt.Print("\r\n")
			return
		}
		if msg == "" {
			continue
		}
		if msg == "/quit" {
			fmt.Print("\r\n")
			return
		}

		// Switch to cooked mode so agent output renders normally.
		term.Restore(fd, state)
		fmt.Print("\r\n")

		ctx, cancel := context.WithCancel(context.Background())
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			select {
			case <-sigCh:
				cancel()
			case <-ctx.Done():
			}
		}()

		pollErr := startAndPoll(ctx, baseURL, apiKey, msg)
		cancel()
		signal.Stop(sigCh)

		if pollErr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", pollErr)
		}
		fmt.Println()

		// Re-enter raw mode for next input.
		state, _ = term.MakeRaw(fd)
		fmt.Print("\x1b[?2004h")
	}
}

// replPipe handles non-terminal stdin (pipes, scripts): one line = one message.
func replPipe(baseURL, apiKey string) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		msg := strings.TrimSpace(scanner.Text())
		if msg == "" || msg == "/quit" {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		if err := startAndPoll(ctx, baseURL, apiKey, msg); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		cancel()
		fmt.Println()
	}
}

// readMessage reads one message in raw mode with controlled echo.
// Typed characters are echoed. Paste markers are replaced with [ … ].
// Returns ("", false) on EOF or Ctrl+D.
func readMessage(r *bufio.Reader) (string, bool) {
	fmt.Print("> ")
	var line []byte   // current typed line
	var acc  []string // lines from previous paste blocks

	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", false
		}
		switch {
		case b == '\r': // Enter — send
			if len(line) > 0 {
				acc = append(acc, string(line))
			}
			return strings.TrimSpace(strings.Join(acc, "\n")), true

		case b == '\x7f' || b == '\b': // Backspace
			if len(line) > 0 {
				line = line[:len(line)-1]
				fmt.Print("\b \b")
			}

		case b == '\x03': // Ctrl+C — clear current input
			line = nil
			acc = nil
			fmt.Print("^C\r\n> ")

		case b == '\x04': // Ctrl+D — quit
			return "", false

		case b == '\x1b': // Escape sequence
			if seq := readEscSeq(r); seq == "[200~" {
				line = collectPaste(r, line, &acc)
			}
			// Other sequences (arrow keys etc.) are ignored.

		case b >= 0x20: // Printable character
			line = append(line, b)
			fmt.Printf("%c", b)
		}
	}
}

// collectPaste reads a bracketed paste block, appends completed lines to acc,
// and returns the remainder as the new current line.
// readUntilPasteEnd echoes content as it arrives, so this function only
// builds the message data structures.
func collectPaste(r *bufio.Reader, line []byte, acc *[]string) []byte {
	if len(line) > 0 {
		*acc = append(*acc, string(line))
	}

	paste, _ := readUntilPasteEnd(r)
	// Discard the \r or \n terminals append after \x1b[201~.
	if b, err := r.ReadByte(); err == nil && b != '\r' && b != '\n' {
		r.UnreadByte()
	}

	lines := strings.Split(strings.TrimRight(paste, "\r\n"), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if i < len(lines)-1 {
			*acc = append(*acc, l)
		} else {
			return []byte(l)
		}
	}
	return nil
}

// readEscSeq reads the bytes of an escape sequence after the leading \x1b.
// '[' is the CSI introducer and is not a final byte; all other bytes in
// 0x40–0x7E terminate the sequence.
func readEscSeq(r *bufio.Reader) string {
	var seq []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			break
		}
		seq = append(seq, b)
		if b >= 0x40 && b <= 0x7E && b != '[' {
			break
		}
	}
	return string(seq)
}

// readUntilPasteEnd reads bytes until the paste-end marker \x1b[201~,
// echoing printable content to the terminal as it arrives.
func readUntilPasteEnd(r *bufio.Reader) (string, error) {
	var buf []byte
	prevCR := false
	for {
		b, err := r.ReadByte()
		if err != nil {
			return string(buf), err
		}
		if b == '\x1b' {
			// Peek ahead to detect the paste-end marker without consuming it.
			peek, _ := r.Peek(5)
			if len(peek) == 5 && bytes.Equal(peek, []byte("[201~")) {
				r.Discard(5)
				return string(buf), nil
			}
			// ESC in paste body — store but don't echo.
			buf = append(buf, b)
			prevCR = false
			continue
		}
		buf = append(buf, b)
		switch {
		case b == '\r':
			prevCR = true
			fmt.Print("\r\n")
		case b == '\n':
			if !prevCR {
				fmt.Print("\r\n")
			}
			prevCR = false
		case b >= 0x20:
			prevCR = false
			fmt.Printf("%c", b)
		default:
			prevCR = false
		}
	}
}

type startResponse struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Error     string `json:"error"`
}

type iterationStartEvent struct {
	Iteration int `json:"iteration"`
}

type doneEvent struct {
	Status               string  `json:"status"`
	SuccessfulIterations int     `json:"successful_iterations"`
	ElapsedSeconds       float64 `json:"elapsed_seconds"`
	Error                string  `json:"error"`
	Output               string  `json:"output"`
}

func startAndPoll(ctx context.Context, baseURL, apiKey, message string) error {
	// Start session
	body := fmt.Sprintf(`{"message":%s}`, jsonString(message))
	resp, err := apiRequest(ctx, http.MethodPost, baseURL+"/agent", apiKey, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /agent: %w", err)
	}
	defer resp.Body.Close()

	// Auth commands return 200 text/event-stream — read the SSE inline.
	if resp.Header.Get("Content-Type") == "text/event-stream" {
		return readSSEStream(resp.Body)
	}

	// Commander commands return 200 OK with a reply directly — no streaming needed.
	if resp.StatusCode == http.StatusOK {
		var result struct {
			Reply string `json:"reply"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return fmt.Errorf("decode reply: %w", err)
		}
		fmt.Println(result.Reply)
		return nil
	}

	var start startResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		return fmt.Errorf("decode start response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST /agent returned %d: %s", resp.StatusCode, start.Error)
	}

	sid := start.SessionID
	fmt.Printf("[queued] session %s\n", sid)

	return streamSession(ctx, baseURL, apiKey, sid)
}

// streamSession connects to GET /agent/{id}/stream and renders events to stdout.
func streamSession(ctx context.Context, baseURL, apiKey, sessionID string) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet,
		baseURL+"/agent/"+sessionID+"/stream", nil)
	if err != nil {
		return err
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}

	// Long-lived connection — no timeout on the client transport
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET /agent/%s/stream: %w", sessionID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream returned %d", resp.StatusCode)
	}

	// Handle Ctrl+C: send stop signal then keep reading until done event
	stopSent := false
	go func() {
		<-ctx.Done()
		if !stopSent {
			stopSent = true
			fmt.Println("\n[stopping] sending stop signal...")
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			stopResp, stopErr := apiRequest(stopCtx, http.MethodPost,
				baseURL+"/agent/"+sessionID+"/stop", apiKey, nil)
			if stopErr == nil {
				stopResp.Body.Close()
			}
		}
	}()

	return readSSEStream(resp.Body)
}

// readSSEStream reads an SSE stream from r until a done event or EOF.
func readSSEStream(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	var eventType, dataLine string

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		case line == "":
			if eventType == "" {
				continue
			}
			done := handleSSEEvent(eventType, dataLine)
			eventType, dataLine = "", ""
			if done {
				return nil
			}
		}
		// ":" prefix = heartbeat comment — ignore
	}
	return scanner.Err()
}

// handleSSEEvent prints progress for known events. Returns true when the session is done.
func handleSSEEvent(eventType, data string) (done bool) {
	switch eventType {
	case "output":
		var e struct {
			Text string `json:"text"`
		}
		json.Unmarshal([]byte(data), &e)
		fmt.Println(e.Text)
	case "iteration_start":
		var e iterationStartEvent
		json.Unmarshal([]byte(data), &e)
		fmt.Printf("[running] iteration %d...\n", e.Iteration)
	case "iteration_done":
		// silent — wait for the done event to print output
	case "done":
		var e doneEvent
		json.Unmarshal([]byte(data), &e)
		if e.Status == "failed" {
			fmt.Printf("[failed] %s\n", e.Error)
		} else {
			fmt.Printf("[completed] %d iterations, %.0fs\n", e.SuccessfulIterations, e.ElapsedSeconds)
		}
		if e.Output != "" {
			fmt.Printf("\nOutput:\n%s\n", e.Output)
		}
		return true
	}
	return false
}

func apiRequest(ctx context.Context, method, url, apiKey string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	return http.DefaultClient.Do(req)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
