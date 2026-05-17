package main

import (
	"bufio"
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

	"github.com/chzyer/readline"
	"github.com/joho/godotenv"
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

	fmt.Printf("agent-cli connected to %s (env: %s)\n", baseURL, *envFile)
	repl(baseURL, apiKey)
}

func repl(baseURL, apiKey string) {
	rl, err := readline.New("> ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "readline init failed: %v\n", err)
		return
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			// Ctrl+D (io.EOF) or Ctrl+C (readline.ErrInterrupt) at empty prompt
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "/quit" {
			return
		}

		ctx, cancel := context.WithCancel(context.Background())

		// Catch Ctrl+C to stop the running session, not exit the REPL
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		go func() {
			select {
			case <-sigCh:
				cancel()
			case <-ctx.Done():
			}
		}()

		err = startAndPoll(ctx, baseURL, apiKey, line)
		cancel()
		signal.Stop(sigCh)

		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		fmt.Println()
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
