package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrDead reports that the pi process exited (or its stdout closed) while
// the client still needed it.
var ErrDead = errors.New("pi process died")

// Options configures a Client.
type Options struct {
	Binary    string // pi binary; default "pi" (tests point this at a fake)
	Provider  string
	Model     string
	Workspace string   // process cwd; the agent's working directory
	Env       []string // overlay on os.Environ()
}

// Result is the outcome of one prompt.
type Result struct {
	Text    string
	CostUSD float64
}

// Event is a normalized progress event. Kinds: prompt_start, tool_start,
// tool_end, retry, compaction, settled, warning.
type Event struct {
	Seq  uint64
	Kind string
	Text string
	At   time.Time
}

// Client owns one pi RPC process. One prompt may be in flight at a time.
type Client struct {
	opts  Options
	cmd   *exec.Cmd
	stdin io.WriteCloser

	writeMu sync.Mutex // serializes stdin writes

	events chan Event

	mu      sync.Mutex
	pending map[string]chan line // command acks keyed by request id
	nextID  int
	settled chan struct{} // closed when the in-flight prompt settles
	text    string        // last assistant text seen via message_end
	cost    float64       // accumulated cost from usage payloads
	seq     uint64
	dead    bool
	deadErr error

	deadCh    chan struct{} // closed when the reader loop ends
	closeOnce sync.Once

	stderr *ringBuffer
	raw    *ringBuffer
}

// Start spawns the pi process bound to the workspace.
func Start(opts Options) (*Client, error) {
	if opts.Binary == "" {
		opts.Binary = "pi"
	}
	args := []string{"--mode", "rpc", "--no-session"}
	if opts.Provider != "" {
		args = append(args, "--provider", opts.Provider)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	cmd := exec.Command(opts.Binary, args...)
	cmd.Dir = opts.Workspace
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	// Put the process in its own group so SIGKILL reaches all children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	c := &Client{
		opts:    opts,
		cmd:     cmd,
		events:  make(chan Event, 256),
		pending: make(map[string]chan line),
		deadCh:  make(chan struct{}),
		stderr:  newRingBuffer(64 * 1024),
		raw:     newRingBuffer(256 * 1024),
	}
	cmd.Stderr = c.stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	c.stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop(stdout)
	return c, nil
}

// Events returns the progress event channel. It is closed when the process
// dies or the client is closed. Slow consumers lose events rather than
// blocking the reader.
func (c *Client) Events() <-chan Event { return c.events }

// StderrTail returns the last 64KB of the process's stderr.
func (c *Client) StderrTail() string { return c.stderr.String() }

// RawTail returns the tail of the raw JSONL transcript (for execution logs).
func (c *Client) RawTail() string { return c.raw.String() }

// Prompt sends one prompt and blocks until pi settles, the context ends, or
// the process dies. On context expiry it sends abort, allows a 5s grace for
// settling, then kills the process group.
func (c *Client) Prompt(ctx context.Context, message string) (*Result, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, c.deadError()
	}
	if c.settled != nil {
		c.mu.Unlock()
		return nil, errors.New("pi: prompt already in flight")
	}
	settled := make(chan struct{})
	c.settled = settled
	c.text = ""
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.settled = nil
		c.mu.Unlock()
	}()

	c.emit("prompt_start", "")
	ackCh, err := c.sendCommand("prompt", message)
	if err != nil {
		return nil, err
	}

	for {
		select {
		case l := <-ackCh:
			if l.Success != nil && !*l.Success {
				return nil, fmt.Errorf("pi rejected prompt: %s", l.Error)
			}
			ackCh = nil // ack consumed; keep waiting for settle
		case <-settled:
			return c.collect(ctx)
		case <-c.deadCh:
			return nil, c.deadError()
		case <-ctx.Done():
			c.abortAndReap(settled)
			return nil, ctx.Err()
		}
	}
}

// collect gathers the assistant text after settle: authoritative
// get_last_assistant_text first, accumulated message_end text as fallback.
func (c *Client) collect(ctx context.Context) (*Result, error) {
	var text string
	collectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ctx.Err() == nil {
		if l, err := c.command(collectCtx, "get_last_assistant_text", ""); err == nil {
			text = extractText(l.Data)
		}
	}
	c.mu.Lock()
	if text == "" {
		text = c.text
	}
	cost := c.cost
	c.mu.Unlock()
	return &Result{Text: strings.TrimSpace(text), CostUSD: cost}, nil
}

// abortAndReap tries a graceful abort, then kills the process group.
func (c *Client) abortAndReap(settled chan struct{}) {
	if _, err := c.sendCommand("abort", ""); err == nil {
		select {
		case <-settled:
			return
		case <-c.deadCh:
			return
		case <-time.After(5 * time.Second):
		}
	}
	c.kill()
}

// Steer injects guidance into the in-flight prompt.
func (c *Client) Steer(ctx context.Context, text string) error {
	_, err := c.command(ctx, "steer", text)
	return err
}

// FollowUp queues a message for delivery after the current prompt settles.
func (c *Client) FollowUp(ctx context.Context, text string) error {
	_, err := c.command(ctx, "follow_up", text)
	return err
}

// Abort cancels the in-flight prompt; the session stays usable.
func (c *Client) Abort(ctx context.Context) error {
	_, err := c.command(ctx, "abort", "")
	return err
}

// Close kills the process group and waits briefly for the reader to drain.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.kill()
		select {
		case <-c.deadCh:
		case <-time.After(10 * time.Second):
		}
	})
	return nil
}

func (c *Client) kill() {
	if c.cmd.Process != nil {
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
		_ = c.cmd.Process.Kill()
	}
}

// sendCommand registers an ack channel and writes the command line.
func (c *Client) sendCommand(typ, message string) (chan line, error) {
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, c.deadError()
	}
	c.nextID++
	id := strconv.Itoa(c.nextID)
	ch := make(chan line, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.send(request{ID: id, Type: typ, Message: message}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	return ch, nil
}

// command sends a request and waits for its ack.
func (c *Client) command(ctx context.Context, typ, message string) (line, error) {
	ch, err := c.sendCommand(typ, message)
	if err != nil {
		return line{}, err
	}
	select {
	case l := <-ch:
		if l.Success != nil && !*l.Success {
			return l, fmt.Errorf("pi %s failed: %s", typ, l.Error)
		}
		return l, nil
	case <-ctx.Done():
		return line{}, ctx.Err()
	case <-c.deadCh:
		return line{}, c.deadError()
	}
}

func (c *Client) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// readLoop consumes stdout lines until EOF, then marks the client dead and
// reaps the process.
func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		c.raw.WriteLine(text)
		var l line
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			c.emit("warning", "malformed pi output line skipped")
			continue
		}
		c.dispatch(&l)
	}
	err := scanner.Err()
	waitErr := c.cmd.Wait()
	c.markDead(err, waitErr)
}

func (c *Client) dispatch(l *line) {
	switch l.Type {
	case "response":
		c.mu.Lock()
		ch := c.pending[idKey(l.ID)]
		delete(c.pending, idKey(l.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- *l
		}
	case "agent_settled":
		c.mu.Lock()
		settled := c.settled
		c.settled = nil
		c.mu.Unlock()
		c.emit("settled", "")
		if settled != nil {
			close(settled)
		}
	case "message_end":
		role, text, usage := extractMessage(l.Message)
		cost := extractCost(usage)
		if cost == 0 {
			cost = extractCost(l.Usage)
		}
		c.mu.Lock()
		if role == "assistant" && text != "" {
			c.text = text
		}
		if cost > 0 {
			c.cost += cost
		}
		c.mu.Unlock()
	case "agent_end":
		c.mu.Lock()
		if cost := extractCost(l.Usage); cost > 0 && c.cost == 0 {
			c.cost = cost
		}
		c.mu.Unlock()
	case "tool_execution_start":
		c.emit("tool_start", l.extractToolName())
	case "tool_execution_end":
		c.emit("tool_end", l.extractToolName())
	case "extension_ui_request":
		// Headless: dialogs must be cancelled or pi blocks forever.
		_ = c.send(uiResponse{Type: "extension_ui_response", ID: l.ID, Cancelled: true})
		c.emit("warning", "cancelled interactive pi dialog (headless)")
	case "extension_error":
		c.emit("warning", "pi extension error: "+l.Error)
	default:
		switch {
		case strings.HasPrefix(l.Type, "auto_retry"):
			c.emit("retry", l.Type)
		case strings.HasPrefix(l.Type, "compaction"):
			c.emit("compaction", l.Type)
		}
	}
}

// markDead wakes every waiter exactly once.
func (c *Client) markDead(scanErr, waitErr error) {
	c.mu.Lock()
	if !c.dead {
		c.dead = true
		switch {
		case scanErr != nil:
			c.deadErr = scanErr
		case waitErr != nil:
			c.deadErr = waitErr
		}
		close(c.deadCh)
		close(c.events)
	}
	c.mu.Unlock()
}

func (c *Client) deadError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deadErr != nil {
		return fmt.Errorf("%w: %v", ErrDead, c.deadErr)
	}
	return ErrDead
}

func (c *Client) emit(kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead {
		return
	}
	c.seq++
	// Send under the lock so markDead's close can't race the send; the
	// channel is buffered and the send non-blocking, so this never stalls.
	select {
	case c.events <- Event{Seq: c.seq, Kind: kind, Text: text, At: time.Now()}:
	default: // drop rather than block the reader
	}
}

// ringBuffer keeps the last max bytes written to it.
type ringBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newRingBuffer(max int) *ringBuffer { return &ringBuffer{max: max} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuffer) WriteLine(s string) {
	_, _ = r.Write(append([]byte(s), '\n'))
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}
