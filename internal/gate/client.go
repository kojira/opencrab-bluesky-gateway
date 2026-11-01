package gate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Outcome of an invoke, mapped to the wire like the Rust client: Ok → ok{result}, Rejected →
// err operation_rejected, Indeterminate → close without answering (core marks it unknown).
type Outcome int

const (
	OutcomeOk Outcome = iota
	OutcomeRejected
	OutcomeIndeterminate
)

// Handler does the platform work for frames from core.
type Handler interface {
	// Say posts the turn's text. A nil error means the platform accepted it.
	Say(ctx context.Context, session, text, replyTarget string) error
	// Invoke runs a declared operation.
	Invoke(ctx context.Context, session, operation string, payload json.RawMessage) (any, Outcome)
}

var (
	ErrNotConnected = errors.New("gate: not connected")
	ErrRejected     = errors.New("gate: rejected by core")
)

const (
	reconnectMin = 200 * time.Millisecond
	reconnectMax = 8 * time.Second
	saidTimeout  = 30 * time.Second
)

// Client is one instance's connection to core, reconnecting with backoff until ctx ends.
type Client struct {
	Socket        string
	InstanceID    string
	AgentID       string
	Operations    []any
	FinalDelivery string
	Handler       Handler
	Log           *slog.Logger

	digest string
	seq    atomic.Uint64

	mu        sync.Mutex
	conn      net.Conn
	ready     bool
	lastError string
	pending   map[string]chan *Msg
	wmu       sync.Mutex
}

// SaidError is core's err answer to a said.
type SaidError struct{ Code string }

func (e *SaidError) Error() string { return "core: " + e.Code }

func (c *Client) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Client) nextID(prefix string) string {
	return prefix + ":" + strconv.FormatUint(c.seq.Add(1), 10)
}

// LastError is why the last connection or hello failed ("" after a successful hello).
func (c *Client) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastError
}

func (c *Client) setError(msg string) {
	c.mu.Lock()
	c.lastError = msg
	c.mu.Unlock()
}

// Ready reports whether hello was accepted on the current connection.
func (c *Client) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready
}

// Run connects, says hello, serves frames, and reconnects until ctx is done.
func (c *Client) Run(ctx context.Context) {
	d, err := DeclarationDigest(c.Operations, c.FinalDelivery)
	if err != nil {
		c.logger().Error("operation declarations", "err", err)
		return
	}
	c.digest = d
	backoff := reconnectMin
	for ctx.Err() == nil {
		helloOK, err := c.session(ctx)
		if err != nil && ctx.Err() == nil {
			c.logger().Warn("gate connection ended", "instance_id", c.InstanceID, "err", err)
			if !helloOK {
				c.setError(err.Error())
			}
		}
		if helloOK {
			backoff = reconnectMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

func (c *Client) send(v any) error {
	b, err := EncodeFrame(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = conn.Write(b)
	return err
}

func (c *Client) session(ctx context.Context) (helloOK bool, err error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return false, err
	}
	helloID := "hello:" + c.InstanceID
	helloCh := make(chan *Msg, 1)
	c.mu.Lock()
	c.conn = conn
	c.ready = false
	c.pending = map[string]chan *Msg{helloID: helloCh}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.ready = false
		for _, ch := range c.pending {
			close(ch)
		}
		c.pending = nil
		c.mu.Unlock()
		conn.Close()
	}()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	if err := c.send(HelloFrame(helloID, c.InstanceID, c.AgentID, c.Operations, c.FinalDelivery)); err != nil {
		return false, err
	}
	c.logger().Info("hello", "instance_id", c.InstanceID)

	// Requests from core are handled one at a time in arrival order; responses bypass them.
	// On return the connection is closed first (ending the reader), then the worker is told to
	// stop; work is never closed, so a late send cannot panic.
	work := make(chan *Msg, 32)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			case m := <-work:
				if c.handleRequest(ctx, m) {
					conn.Close()
				}
			}
		}
	}()
	defer func() { conn.Close(); close(quit); <-done }()

	readErr := make(chan error, 1)
	go func() {
		r := bufio.NewReaderSize(conn, 64<<10)
		for {
			frame, err := ReadFrame(r)
			if err != nil {
				readErr <- err
				return
			}
			m, err := ParseMsg(frame)
			if err != nil {
				readErr <- err
				return
			}
			if m.M == "ok" || m.M == "err" {
				if !c.deliver(m) {
					readErr <- fmt.Errorf("gate: response to unknown id %q", m.IDString())
					return
				}
				continue
			}
			select {
			case work <- m:
			default:
				readErr <- errors.New("gate: request queue full")
				return
			}
		}
	}()

	select {
	case m, ok := <-helloCh:
		if !ok {
			return false, <-readErr
		}
		if m.M != "ok" {
			c.logger().Warn("hello failed", "instance_id", c.InstanceID, "reason", m.Code)
			return false, &SaidError{Code: m.Code}
		}
	case err := <-readErr:
		return false, err
	case <-time.After(15 * time.Second):
		return false, errors.New("gate: hello timed out")
	}
	c.mu.Lock()
	c.ready = true
	c.lastError = ""
	c.mu.Unlock()
	c.logger().Info("hello ok", "instance_id", c.InstanceID)
	return true, <-readErr
}

func (c *Client) deliver(m *Msg) bool {
	c.mu.Lock()
	ch, ok := c.pending[m.IDString()]
	if ok {
		delete(c.pending, m.IDString())
	}
	c.mu.Unlock()
	if ok {
		ch <- m
	}
	return ok
}

// handleRequest answers one frame from core; true means close the connection.
func (c *Client) handleRequest(ctx context.Context, m *Msg) bool {
	id := m.IDString()
	switch m.M {
	case "say":
		text, target := SayText(m.Payload)
		if text == "" {
			_ = c.send(ErrFrame(id, "external_rejected"))
			return false
		}
		if err := c.Handler.Say(ctx, m.Session, text, target); err != nil {
			c.logger().Warn("say not delivered", "instance_id", c.InstanceID, "err", err)
			_ = c.send(ErrFrame(id, "external_rejected"))
			return false
		}
		_ = c.send(OkFrame(id))
		return false
	case "invoke":
		if code := c.validateInvoke(m); code != "" {
			_ = c.send(ErrFrame(id, code))
			return false
		}
		result, outcome := c.Handler.Invoke(ctx, m.Session, m.Operation, m.Payload)
		switch outcome {
		case OutcomeOk:
			_ = c.send(InvokeOkFrame(id, result))
			return false
		case OutcomeRejected:
			_ = c.send(ErrFrame(id, "operation_rejected"))
			return false
		default:
			return true
		}
	default:
		// Notifications (activity, partial, tool_activity, ...) need no answer. An unknown
		// request with an id is answered so core does not wait.
		if id != "" && m.M != "activity" {
			_ = c.send(ErrFrame(id, "unknown_message"))
		}
		return false
	}
}

func (c *Client) validateInvoke(m *Msg) string {
	if m.InvocationProtocol != 1 || m.Session == "" || m.IDString() == "" {
		return "bad_request"
	}
	var decl Obj
	for _, op := range c.Operations {
		if o, ok := op.(Obj); ok && o["name"] == m.Operation {
			decl = o
		}
	}
	if decl == nil {
		return "operation_unknown"
	}
	if m.DeclarationDigest != c.digest || decl["dispatch"] != m.Dispatch || decl["effect"] != m.Effect {
		return "operation_rejected"
	}
	return ""
}

func (c *Client) request(ctx context.Context, prefix string, build func(id string) Obj) (*Msg, error) {
	id := c.nextID(prefix)
	ch := make(chan *Msg, 1)
	c.mu.Lock()
	if !c.ready || c.pending == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.send(build(id)); err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		if m.M != "ok" {
			return nil, &SaidError{Code: m.Code}
		}
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(saidTimeout):
		return nil, errors.New("gate: no answer from core")
	}
}

// Said records one inbound message. seq is 0 when core did not admit it.
func (c *Client) Said(ctx context.Context, s Said) (seq int64, err error) {
	m, err := c.request(ctx, "said", func(id string) Obj { return SaidFrame(id, s) })
	if err != nil {
		return 0, err
	}
	if len(m.Seq) > 0 && string(m.Seq) != "null" {
		_ = json.Unmarshal(m.Seq, &seq)
	}
	return seq, nil
}

// Open makes sure core has the session of a key; it returns core's session id.
func (c *Client) Open(ctx context.Context, session string) (string, error) {
	m, err := c.request(ctx, "open", func(id string) Obj { return OpenFrame(id, session) })
	if err != nil {
		return "", err
	}
	return m.SessionID, nil
}
