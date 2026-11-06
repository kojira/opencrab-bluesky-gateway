package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
	"github.com/kojira/opencrab-bluesky-gateway/internal/store"
)

func newDaemon(t *testing.T) *Daemon {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Config: Config{CoreSocket: "/nonexistent.sock"}, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	return d
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestSettingsAPINeverReturnsThePassword(t *testing.T) {
	d := newDaemon(t)
	h := d.Handler()
	code, body := do(t, h, "POST", "/api/bots", `{"agent_id":"agent-1","label":"Blueskyちゃん"}`)
	if code != 201 || !strings.Contains(body, `"configured":false`) {
		t.Fatalf("create %d %s", code, body)
	}
	var created struct {
		InstanceID string `json:"instance_id"`
	}
	_ = json.Unmarshal([]byte(body), &created)
	if created.InstanceID != store.InstanceID("agent-1") {
		t.Fatal("instance id is not derived from the agent")
	}
	code, body = do(t, h, "PATCH", "/api/bots/"+created.InstanceID, `{"handle":"@bot.test","app_password":"secret-app-pass","enabled":false}`)
	if code != 200 || strings.Contains(body, "secret-app-pass") || !strings.Contains(body, `"app_password_set":true`) || !strings.Contains(body, `"handle":"bot.test"`) {
		t.Fatalf("patch %d %s", code, body)
	}
	_, body = do(t, h, "GET", "/api/bots", "")
	if strings.Contains(body, "secret-app-pass") || strings.Contains(body, "app_password\"") {
		t.Fatalf("list leaks: %s", body)
	}
	// an empty password keeps the stored one
	do(t, h, "PATCH", "/api/bots/"+created.InstanceID, `{"app_password":""}`)
	b, _ := d.Store.Get(created.InstanceID)
	if b.AppPassword != "secret-app-pass" {
		t.Fatal("empty password replaced the stored one")
	}
	// the file holding it is 0600
	fi, err := os.Stat(store.PathOf(d.Store))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("store mode %v %v", fi.Mode(), err)
	}
	if code, _ := do(t, h, "POST", "/api/bots", `{"agent_id":"agent-1"}`); code != 409 {
		t.Fatalf("duplicate agent %d", code)
	}
	if code, _ := do(t, h, "PATCH", "/api/bots/"+created.InstanceID, `{"poll_secs":1}`); code != 400 {
		t.Fatalf("poll_secs 1 accepted: %d", code)
	}
	if code, _ := do(t, h, "DELETE", "/api/bots/"+created.InstanceID, ""); code != 200 {
		t.Fatal("delete")
	}
	if _, body := do(t, h, "GET", "/api/bots", ""); !strings.Contains(body, `"bots":[]`) {
		t.Fatalf("after delete %s", body)
	}
	if code, body := do(t, h, "GET", "/", ""); code != 200 || !strings.Contains(body, "Bluesky gateway 設定") {
		t.Fatalf("page %d", code)
	}
}

// --- delivery ---

type fakeBsky struct {
	page *bsky.NotificationPage
}

func (f *fakeBsky) ListNotifications(context.Context, int) (*bsky.NotificationPage, error) {
	return f.page, nil
}
func (f *fakeBsky) SelfDID(context.Context) (string, error) { return "did:plc:bot", nil }

type fakeSink struct {
	got  []gate.Said
	fail error
}

func (f *fakeSink) Ready() bool { return true }
func (f *fakeSink) Said(_ context.Context, s gate.Said) (int64, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	f.got = append(f.got, s)
	return int64(len(f.got)), nil
}

func n(uri, did, at string) bsky.Notification {
	return bsky.Notification{URI: uri, Author: bsky.Author{DID: did, Handle: "a.test"}, Reason: "mention", IndexedAt: at, Record: json.RawMessage(`{"text":"` + uri + `"}`)}
}

func TestDeliverStartsFromNowSkipsOwnPostsAndSurvivesRestart(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	b, _ := st.Create("agent-1", "bot")
	bs := &fakeBsky{page: &bsky.NotificationPage{Notifications: []bsky.Notification{n("at://old", "did:plc:alice", "2026-01-01T00:00:00Z")}}}
	sink := &fakeSink{}
	ctx := context.Background()
	if err := Deliver(ctx, st, b, bs, sink); err != nil || len(sink.got) != 0 {
		t.Fatalf("first poll delivered %v %v", sink.got, err)
	}
	bs.page.Notifications = []bsky.Notification{
		n("at://own", "did:plc:bot", "2099-01-01T00:00:03Z"),
		n("at://new2", "did:plc:alice", "2099-01-01T00:00:02Z"),
		n("at://new1", "did:plc:alice", "2099-01-01T00:00:01Z"),
		n("at://old", "did:plc:alice", "2026-01-01T00:00:00Z"),
	}
	if err := Deliver(ctx, st, b, bs, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.got) != 2 || sink.got[0].Origin != "at://new1" || sink.got[1].Origin != "at://new2" {
		t.Fatalf("delivered %+v", sink.got)
	}
	// a new process on the same store delivers nothing again
	st2, _ := store.Open(filepath.Dir(store.PathOf(st)))
	sink2 := &fakeSink{}
	if err := Deliver(ctx, st2, b, bs, sink2); err != nil || len(sink2.got) != 0 {
		t.Fatalf("repeated after restart: %+v %v", sink2.got, err)
	}
}

func TestDeliverKeepsUndeliveredWhenCoreIsAway(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	b, _ := st.Create("agent-1", "bot")
	bs := &fakeBsky{page: &bsky.NotificationPage{}}
	ctx := context.Background()
	_ = Deliver(ctx, st, b, bs, &fakeSink{})
	bs.page.Notifications = []bsky.Notification{n("at://x", "did:plc:alice", "2099-01-01T00:00:01Z")}
	if err := Deliver(ctx, st, b, bs, &fakeSink{fail: gate.ErrNotConnected}); err == nil {
		t.Fatal("no error when core is away")
	}
	sink := &fakeSink{}
	if err := Deliver(ctx, st, b, bs, sink); err != nil || len(sink.got) != 1 {
		t.Fatalf("not delivered later: %+v %v", sink.got, err)
	}
}

// --- operations ---

type fakePoster struct {
	Reader // reads go to a bsky.Client on the stub XRPC server (read_test.go)
	calls  []string
	err    error
}

func (f *fakePoster) Post(_ context.Context, text, replyTo string, images []bsky.Image) ([]string, error) {
	f.calls = append(f.calls, "post|"+text+"|"+replyTo+"|"+string(rune('0'+len(images))))
	return []string{"at://x"}, f.err
}
func (f *fakePoster) Like(_ context.Context, uri string) (string, error) {
	f.calls = append(f.calls, "like|"+uri)
	return "", f.err
}
func (f *fakePoster) Repost(_ context.Context, uri string) (string, error) {
	f.calls = append(f.calls, "repost|"+uri)
	return "", f.err
}
func (f *fakePoster) Follow(_ context.Context, who string) (string, error) {
	f.calls = append(f.calls, "follow|"+who)
	return "", f.err
}

func TestOperationsAreSortedAndValid(t *testing.T) {
	ops := Operations(true)
	prev := ""
	for _, o := range ops {
		name := o.(gate.Obj)["name"].(string)
		if name <= prev {
			t.Fatalf("not sorted: %s after %s", name, prev)
		}
		prev = name
		dispatch, effect := o.(gate.Obj)["dispatch"], o.(gate.Obj)["effect"]
		if (dispatch == "utterance") != (effect == "utterance") {
			t.Fatalf("%s: dispatch %v effect %v", name, dispatch, effect)
		}
	}
	if _, err := gate.DeclarationDigest(ops, gate.FinalDeliveryOperationDriven); err != nil {
		t.Fatal(err)
	}
}

func TestInvokeRunsTheOperation(t *testing.T) {
	p := &fakePoster{}
	o := &Ops{Bsky: p}
	ctx := context.Background()
	if _, out := o.Invoke(ctx, "s", "reply", json.RawMessage(`{"event":"at://p","text":"hi"}`)); out != gate.OutcomeOk {
		t.Fatal(out)
	}
	if _, out := o.Invoke(ctx, "s", "reply", json.RawMessage(`{"event":"e7","text":"hi"}`)); out != gate.OutcomeRejected {
		t.Fatal("unresolved short ref accepted")
	}
	o.Invoke(ctx, "s", "like", json.RawMessage(`{"event":"at://p"}`))
	o.Invoke(ctx, "s", "follow", json.RawMessage(`{"user":"@alice.test"}`))
	if err := o.Say(ctx, "s", "plain", ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"post|hi|at://p|0", "like|at://p", "follow|@alice.test", "post|plain||0"}
	if strings.Join(p.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls %v", p.calls)
	}
	if _, out := (&Ops{}).Invoke(ctx, "s", "like", json.RawMessage(`{"event":"at://p"}`)); out != gate.OutcomeRejected {
		t.Fatal("a bot without an account must reject")
	}
	p.err = &bsky.APIError{Status: 400, Code: "InvalidRequest"}
	if _, out := o.Invoke(ctx, "s", "like", json.RawMessage(`{"event":"at://p"}`)); out != gate.OutcomeRejected {
		t.Fatal("a 400 must be a rejection")
	}
	p.err = io.ErrUnexpectedEOF
	if _, out := o.Invoke(ctx, "s", "like", json.RawMessage(`{"event":"at://p"}`)); out != gate.OutcomeIndeterminate {
		t.Fatal("a network error must be indeterminate")
	}
}

func TestImagesComeFromVerifiedOutboxCopies(t *testing.T) {
	root := t.TempDir()
	data := []byte("\xff\xd8jpeg")
	_ = os.MkdirAll(filepath.Join(root, "a"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "a", "x.jpg"), data, 0o600)
	sum := sha256.Sum256(data)
	desc := func(sha string) string {
		return `{"id":"1","name":"x.jpg","media_type":"image/jpeg","size":` + itoa(len(data)) + `,"sha256":"` + sha + `","local_path":"a/x.jpg"}`
	}
	p := &fakePoster{}
	o := &Ops{Bsky: p, OutboxRoot: root}
	if _, out := o.Invoke(context.Background(), "s", "post", json.RawMessage(`{"text":"t","files":[`+desc(hex.EncodeToString(sum[:]))+`]}`)); out != gate.OutcomeOk || p.calls[0] != "post|t||1" {
		t.Fatalf("%v %v", out, p.calls)
	}
	if _, out := o.Invoke(context.Background(), "s", "post", json.RawMessage(`{"text":"t","files":[`+desc(strings.Repeat("0", 64))+`]}`)); out != gate.OutcomeRejected {
		t.Fatal("a changed copy was posted")
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// D-1119: a subtask is the agent itself, so every Bluesky op is offered to subtasks too.
func TestEveryOperationIsOfferedToSubtasks(t *testing.T) {
	for _, op := range Operations(true) {
		o := op.(gate.Obj)
		if o["sub_engine"] != "allowed" {
			t.Fatalf("%v: sub_engine = %v, want allowed", o["name"], o["sub_engine"])
		}
	}
}
