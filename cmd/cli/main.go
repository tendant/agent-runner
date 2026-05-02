package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	bind := os.Getenv("API_BIND")
	if bind == "" {
		bind = "127.0.0.1:8080"
	}
	baseURL := "http://" + bind
	apiKey := os.Getenv("API_KEY")

	fmt.Printf("agent-cli connected to %s\n", baseURL)
	repl(baseURL, apiKey)
}

func repl(baseURL, apiKey string) {
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			// Ctrl+D / EOF
			fmt.Println()
			return
		}
		line := strings.TrimSpace(scanner.Text())
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

		err := startAndPoll(ctx, baseURL, apiKey, line)
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

type iterationResult struct {
	Iteration int      `json:"iteration"`
	Status    string   `json:"status"`
	Output    string   `json:"output"`
	Error     string   `json:"error"`
	Files     []string `json:"changed_files"`
	Duration  float64  `json:"duration_seconds"`
}

type pollResponse struct {
	SessionID            string            `json:"session_id"`
	Status               string            `json:"status"`
	CurrentIteration     int               `json:"current_iteration"`
	SuccessfulIterations int               `json:"successful_iterations"`
	ElapsedSeconds       float64           `json:"elapsed_seconds"`
	Error                string            `json:"error"`
	Iterations           []iterationResult `json:"iterations"`
}

func startAndPoll(ctx context.Context, baseURL, apiKey, message string) error {
	// Start session
	body := fmt.Sprintf(`{"message":%s}`, jsonString(message))
	resp, err := apiRequest(ctx, http.MethodPost, baseURL+"/agent", apiKey, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("POST /agent: %w", err)
	}
	defer resp.Body.Close()

	var start startResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		return fmt.Errorf("decode start response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("POST /agent returned %d: %s", resp.StatusCode, start.Error)
	}

	sid := start.SessionID
	fmt.Printf("[queued] session %s\n", sid)

	// Poll loop
	var lastIteration int
	stopSent := false
	pollURL := baseURL + "/agent/" + sid

	for {
		select {
		case <-ctx.Done():
			if !stopSent {
				stopSent = true
				fmt.Println("\n[stopping] sending stop signal...")
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
				stopResp, stopErr := apiRequest(stopCtx, http.MethodPost, pollURL+"/stop", apiKey, nil)
				if stopErr == nil {
					stopResp.Body.Close()
				}
				stopCancel()
			}
		default:
		}

		time.Sleep(2 * time.Second)

		pollCtx, pollCancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := apiRequest(pollCtx, http.MethodGet, pollURL, apiKey, nil)
		pollCancel()
		if err != nil {
			return fmt.Errorf("GET /agent/%s: %w", sid, err)
		}

		var poll pollResponse
		if err := json.NewDecoder(resp.Body).Decode(&poll); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decode poll response: %w", err)
		}
		resp.Body.Close()

		// Print new iteration progress
		for poll.CurrentIteration > lastIteration {
			lastIteration++
			fmt.Printf("[running] iteration %d...\n", lastIteration)
		}

		switch poll.Status {
		case "completed":
			fmt.Printf("[completed] %d iterations, %.0fs\n", poll.SuccessfulIterations, poll.ElapsedSeconds)
			printOutput(poll)
			return nil
		case "failed":
			fmt.Printf("[failed] %s\n", poll.Error)
			printOutput(poll)
			return nil
		case "stopping":
			if !stopSent {
				fmt.Println("[stopping]...")
			}
			// Keep polling until terminal state
		}
	}
}

func printOutput(poll pollResponse) {
	if len(poll.Iterations) == 0 {
		return
	}
	last := poll.Iterations[len(poll.Iterations)-1]
	if last.Output != "" {
		fmt.Printf("\nOutput:\n%s\n", last.Output)
	}
	if last.Error != "" {
		fmt.Printf("\nError:\n%s\n", last.Error)
	}
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
