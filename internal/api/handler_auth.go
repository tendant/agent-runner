package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const authTimeout = 5 * time.Minute

var urlRe = regexp.MustCompile(`https?://\S+`)

// ansiRe matches ANSI/VT100 escape sequences (e.g. \x1b[0m, \x1b[1;32m).
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return strings.TrimSpace(ansiRe.ReplaceAllString(s, ""))
}

// runCLIAuthFlowCtx runs the given CLI's auth command under the provided context
// (which may be cancelled by /auth cancel) with an overall 5-minute timeout.
// URLs found in the output are sent immediately so the user can open them.
// The function blocks until the CLI exits, the context is cancelled, or the timeout fires.
func runCLIAuthFlowCtx(parent context.Context, cli string, send func(string)) {
	ctx, cancel := context.WithTimeout(parent, authTimeout)
	defer cancel()

	var args []string
	switch cli {
	case "claude":
		args = []string{"auth", "login"}
	case "codex":
		// Device code grant (RFC 8628): CLI prints user_code + verification_uri;
		// no browser redirect or local callback server required.
		args = []string{"login", "--device-auth"}
	default:
		send(fmt.Sprintf("error: /auth only supports 'claude' and 'codex' (got %q)", cli))
		return
	}

	cmd := exec.CommandContext(ctx, cli, args...)

	outPR, outPW := io.Pipe()
	errPR, errPW := io.Pipe()
	cmd.Stdout = outPW
	cmd.Stderr = errPW

	if err := cmd.Start(); err != nil {
		send(fmt.Sprintf("error: failed to start %s auth: %v", cli, err))
		return
	}

	done := make(chan struct{}, 2)
	scanLines := func(r io.Reader) {
		defer func() { done <- struct{}{} }()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := stripANSI(scanner.Text())
			if line == "" {
				continue
			}
			slog.Debug("auth output", "cli", cli, "line", line)
			if m := urlRe.FindString(line); m != "" {
				send("Open this URL in your browser to authenticate " + cli + ":\n\n" + m)
			} else {
				send(line)
			}
		}
	}

	go scanLines(outPR)
	go scanLines(errPR)

	<-done
	<-done
	outPW.Close()
	errPW.Close()

	if err := cmd.Wait(); err != nil {
		switch ctx.Err() {
		case context.Canceled:
			send("auth cancelled")
		case context.DeadlineExceeded:
			send("auth timed out (5 min)")
		default:
			send(fmt.Sprintf("%s auth failed: %v", cli, err))
		}
		return
	}
	send(cli + " authenticated successfully.")
}
