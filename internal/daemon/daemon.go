// Package daemon runs one gate client and one notification poller per enabled bot, and the
// settings page.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
	"github.com/kojira/opencrab-bluesky-gateway/internal/inbound"
	"github.com/kojira/opencrab-bluesky-gateway/internal/oauthlink"
	"github.com/kojira/opencrab-bluesky-gateway/internal/store"
)

// Config is the daemon's JSON config file.
type Config struct {
	CoreSocket string `json:"core_socket"`
	DataDir    string `json:"data_dir"`
	Listen     string `json:"listen"`
	// CoreAPI is core's HTTP API (for the agent list on the settings page); optional.
	CoreAPI string `json:"core_api"`
	// AttachmentOutboxRoot is core's outbox; enables images in post/reply when set.
	AttachmentOutboxRoot string `json:"attachment_outbox_root"`
	// OAuth enables 「Bluesky でログイン」 (optional; without it bots use app passwords).
	OAuth *oauthlink.Config `json:"oauth"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if c.CoreSocket == "" || c.DataDir == "" {
		return c, errors.New("config needs core_socket and data_dir")
	}
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8795"
	}
	return c, nil
}

// Status is what the settings page shows for a running bot.
type Status struct {
	Running   bool   `json:"running"`
	Connected bool   `json:"connected"`
	CoreError string `json:"core_error"`
	LoggedIn  bool   `json:"logged_in"`
	LastError string `json:"last_error"`
	LastPoll  string `json:"last_poll"`
}

type runner struct {
	cancel context.CancelFunc
	done   chan struct{}
	client *gate.Client
	mu     sync.Mutex
	st     Status
}

func (r *runner) set(f func(*Status)) {
	r.mu.Lock()
	f(&r.st)
	r.mu.Unlock()
}

// Daemon owns the bots' runners.
type Daemon struct {
	Config Config
	Store  *store.Store
	Log    *slog.Logger
	// NewBsky makes a bot's Bluesky client (replaced in tests).
	NewBsky func(store.Bot) *bsky.Client
	// OAuth links bots with atproto OAuth; nil when the config has no oauth block.
	OAuth *oauthlink.Linker

	mu      sync.Mutex
	runners map[string]*runner
	ctx     context.Context
}

func (d *Daemon) newBsky(b store.Bot) *bsky.Client {
	if d.NewBsky != nil {
		return d.NewBsky(b)
	}
	c := &bsky.Client{Service: b.Service, Handle: b.Handle, Password: b.AppPassword}
	if d.OAuth != nil {
		if sess, err := d.OAuth.Session(context.Background(), b.InstanceID); err == nil {
			c.OAuth = sess
		}
	}
	return c
}

// oauthLinked reports whether the bot has an OAuth session.
func (d *Daemon) oauthLinked(id string) (oauthlink.Link, bool) {
	if d.OAuth == nil {
		return oauthlink.Link{}, false
	}
	return d.OAuth.Store.Linked(id)
}

// configured: the bot can reach its account (OAuth session, or handle and app password).
func (d *Daemon) configured(b store.Bot) bool {
	_, linked := d.oauthLinked(b.InstanceID)
	return linked || b.Configured()
}

// Start runs every enabled, configured bot.
func (d *Daemon) Start(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.runners = map[string]*runner{}
	d.mu.Unlock()
	for _, b := range d.Store.List() {
		d.Apply(b.InstanceID)
	}
}

// Apply restarts one bot after its settings changed (or stops it when deleted or disabled).
// Other bots are not touched.
func (d *Daemon) Apply(id string) {
	d.mu.Lock()
	old := d.runners[id]
	delete(d.runners, id)
	ctx := d.ctx
	d.mu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}
	// An enabled bot always connects to core; it reads Bluesky only once it has credentials.
	b, err := d.Store.Get(id)
	if err != nil || !b.Enabled || ctx == nil {
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	bs := d.newBsky(b)
	ops := &Ops{OutboxRoot: d.Config.AttachmentOutboxRoot, AgentID: b.AgentID}
	if d.configured(b) {
		ops.Bsky = bs
	}
	r := &runner{cancel: cancel, done: make(chan struct{})}
	r.client = &gate.Client{
		Socket:        d.Config.CoreSocket,
		InstanceID:    b.InstanceID,
		AgentID:       b.AgentID,
		Operations:    Operations(d.Config.AttachmentOutboxRoot != ""),
		FinalDelivery: gate.FinalDeliveryOperationDriven,
		Handler:       ops,
		Log:           d.Log.With("bot", b.Label),
	}
	r.set(func(s *Status) { s.Running = true })
	d.mu.Lock()
	d.runners[id] = r
	d.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.client.Run(rctx) }()
	if d.configured(b) {
		wg.Add(1)
		go func() { defer wg.Done(); d.poll(rctx, b, bs, r) }()
	}
	go func() { wg.Wait(); close(r.done) }()
}

// Stop ends every runner.
func (d *Daemon) Stop() {
	d.mu.Lock()
	rs := d.runners
	d.runners = map[string]*runner{}
	d.mu.Unlock()
	for _, r := range rs {
		r.cancel()
		<-r.done
	}
}

func (d *Daemon) Status(id string) Status {
	d.mu.Lock()
	r := d.runners[id]
	d.mu.Unlock()
	if r == nil {
		return Status{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	st.Connected = r.client.Ready()
	st.CoreError = r.client.LastError()
	return st
}

// poll fetches notifications every PollSecs and gives new mentions, replies and quotes to core.
func (d *Daemon) poll(ctx context.Context, b store.Bot, bs *bsky.Client, r *runner) {
	log := d.Log.With("bot", b.Label)
	interval := time.Duration(b.PollSecs) * time.Second
	if interval <= 0 {
		interval = store.DefaultPollSecs * time.Second
	}
	for {
		err := d.pollOnce(ctx, b, bs, r.client)
		r.set(func(s *Status) {
			s.LastPoll = time.Now().Format(time.RFC3339)
			s.LoggedIn = bs.LoggedIn()
			if err != nil {
				s.LastError = err.Error()
			} else {
				s.LastError = ""
			}
		})
		if err != nil && ctx.Err() == nil {
			log.Warn("poll", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Sink receives saids (the gate client; a stub in tests).
type Sink interface {
	Said(ctx context.Context, s gate.Said) (int64, error)
	Ready() bool
}

// Notifier lists a bot's notifications (the Bluesky client; a stub in tests).
type Notifier interface {
	ListNotifications(ctx context.Context, limit int) (*bsky.NotificationPage, error)
	SelfDID(ctx context.Context) (string, error)
}

// DMReader reads the bot's DM event log (the Bluesky client; a stub in tests).
type DMReader interface {
	DMLog(ctx context.Context, cursor string) (*bsky.DMLog, error)
}

func (d *Daemon) pollOnce(ctx context.Context, b store.Bot, bs *bsky.Client, sink Sink) error {
	if err := Deliver(ctx, d.Store, b, bs, sink); err != nil {
		return err
	}
	if !bs.CanDM() {
		return nil
	}
	return DeliverDMs(ctx, d.Store, b, bs, bs, sink, d.Log.With("bot", b.Label))
}

// DeliverDMs is one DM poll: new messages of the event log go to core in order, owners' only;
// the cursor moves page by page, so a restart neither repeats nor skips them. The first
// run starts from now (old DMs are not answered). Core being away stops the round.
func DeliverDMs(ctx context.Context, st *store.Store, b store.Bot, self Notifier, dms DMReader, sink Sink, log *slog.Logger) error {
	if !sink.Ready() {
		return nil
	}
	selfDID, err := self.SelfDID(ctx)
	if err != nil {
		return err
	}
	cur := st.Cursor(b.InstanceID)
	if cur.DMLog == "" {
		page, err := dms.DMLog(ctx, "")
		if err != nil {
			return err
		}
		cur.DMLog = page.Cursor
		if cur.DMLog == "" {
			return nil
		}
		return st.SetCursor(b.InstanceID, cur)
	}
	for {
		page, err := dms.DMLog(ctx, cur.DMLog)
		if err != nil {
			return err
		}
		for _, e := range page.Logs {
			if said, ok := inbound.MapDM(e, selfDID, b.AgentID, b.Owners); ok {
				if _, err := sink.Said(ctx, said); err != nil {
					var se *gate.SaidError
					if !errors.As(err, &se) {
						return err // not delivered: try again next round
					}
				}
			} else if m, isMsg := e.Message(); isMsg && m.Sender.DID != selfDID {
				log.Debug("dm not delivered (not an owner or empty)", "convo", e.ConvoID)
			}
		}
		// The cursor moves once the page is handled. A page cut short by a crash is read again;
		// core drops a said whose origin it already has, so nothing is delivered twice.
		if page.Cursor != "" && page.Cursor != cur.DMLog {
			cur.DMLog = page.Cursor
			if err := st.SetCursor(b.InstanceID, cur); err != nil {
				return err
			}
		}
		if len(page.Logs) == 0 {
			return nil
		}
	}
}

// Deliver is one poll: new notifications go to core in order; each one handled moves the
// cursor, so a restart neither repeats nor skips them. Core being away stops the round.
func Deliver(ctx context.Context, st *store.Store, b store.Bot, bs Notifier, sink Sink) error {
	if !sink.Ready() {
		return nil
	}
	self, err := bs.SelfDID(ctx)
	if err != nil {
		return err
	}
	page, err := bs.ListNotifications(ctx, 50)
	if err != nil {
		return err
	}
	cur := st.Cursor(b.InstanceID)
	if cur.Since == "" {
		cur.Start(page.Notifications)
		return st.SetCursor(b.InstanceID, cur)
	}
	for _, n := range cur.Pending(page.Notifications) {
		if said, ok := inbound.Map(n, self, b.AgentID, b.Owners); ok {
			if _, err := sink.Said(ctx, said); err != nil {
				var se *gate.SaidError
				if !errors.As(err, &se) {
					return err // not delivered: try again next round
				}
			}
		}
		cur.Done(n)
		if err := st.SetCursor(b.InstanceID, cur); err != nil {
			return err
		}
	}
	return nil
}
