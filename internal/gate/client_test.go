package gate

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type recordingHandler struct {
	said    chan string
	invoked chan string
}

func (h *recordingHandler) Say(_ context.Context, session, text, _ string) error {
	h.said <- session + "|" + text
	return nil
}

func (h *recordingHandler) Invoke(_ context.Context, _ string, op string, _ json.RawMessage) (any, Outcome) {
	h.invoked <- op
	return Obj{"done": true}, OutcomeOk
}

// fakeCore accepts one connection and lets the test read and write frames.
type fakeCore struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (f *fakeCore) read() map[string]any {
	f.t.Helper()
	_ = f.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := ReadFrame(f.r)
	if err != nil {
		f.t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fakeCore) write(v any) {
	b, _ := EncodeFrame(v)
	if _, err := f.conn.Write(b); err != nil {
		f.t.Fatal(err)
	}
}

func startCore(t *testing.T) (string, chan *fakeCore) {
	dir, err := os.MkdirTemp("/tmp", "gate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "g.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan *fakeCore, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- &fakeCore{t: t, conn: c, r: bufio.NewReader(c)}
		}
	}()
	return sock, ch
}

func TestClientHelloSaidSayInvoke(t *testing.T) {
	sock, cores := startCore(t)
	ops := []any{Obj{
		"name": "like", "description": "d", "input_schema": Obj{"type": "object"},
		"output_schema": nil, "callback_schema": nil,
		"authorization": Obj{"allowed_callers": []any{"owner"}},
		"dispatch":      "utterance", "sub_engine": "not_exposed", "sharing": "conversation_bound", "effect": "utterance",
	}}
	h := &recordingHandler{said: make(chan string, 1), invoked: make(chan string, 1)}
	c := &Client{Socket: sock, InstanceID: "inst", AgentID: "agent", Operations: ops, FinalDelivery: FinalDeliveryOperationDriven, Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	core := <-cores
	hello := core.read()
	if hello["m"] != "hello" || hello["protocol"] != float64(3) || hello["agent_id"] != "agent" {
		t.Fatalf("hello %v", hello)
	}
	core.write(OkFrame(hello["id"].(string)))
	deadline := time.Now().Add(3 * time.Second)
	for !c.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// said → ok{seq}
	done := make(chan int64, 1)
	go func() {
		seq, err := c.Said(ctx, Said{Session: "bluesky-agent", Origin: "at://x", AuthorID: "did:x", Text: "hi"})
		if err != nil {
			t.Error(err)
		}
		done <- seq
	}()
	said := core.read()
	if said["m"] != "said" || said["session"] != "bluesky-agent" {
		t.Fatalf("said %v", said)
	}
	core.write(Obj{"id": said["id"], "m": "ok", "seq": 7, "session_id": "s1"})
	if seq := <-done; seq != 7 {
		t.Fatalf("seq %d", seq)
	}

	// say → handler, ok
	core.write(Obj{"id": "d1", "m": "say", "session": "bluesky-agent", "payload": Obj{"text": "hello"}})
	if got := <-h.said; got != "bluesky-agent|hello" {
		t.Fatal(got)
	}
	if ack := core.read(); ack["id"] != "d1" || ack["m"] != "ok" {
		t.Fatalf("say ack %v", ack)
	}

	// invoke with a stale digest is refused without running the handler
	core.write(Obj{"id": "c1", "m": "invoke", "invocation_protocol": 1, "session": "bluesky-agent", "declaration_digest": "0000000000000000000000000000000000000000000000000000000000000000", "operation": "like", "dispatch": "utterance", "effect": "utterance", "payload": Obj{}})
	if r := core.read(); r["m"] != "err" || r["code"] != "operation_rejected" {
		t.Fatalf("stale digest %v", r)
	}
	digest, _ := DeclarationDigest(ops, FinalDeliveryOperationDriven)
	core.write(Obj{"id": "c2", "m": "invoke", "invocation_protocol": 1, "session": "bluesky-agent", "declaration_digest": digest, "operation": "like", "dispatch": "utterance", "effect": "utterance", "payload": Obj{"event": "at://p"}})
	if op := <-h.invoked; op != "like" {
		t.Fatal(op)
	}
	if r := core.read(); r["id"] != "c2" || r["m"] != "ok" {
		t.Fatalf("invoke %v", r)
	}
	// unknown request with an id is answered
	core.write(Obj{"id": "q1", "m": "command_v9", "session": "x"})
	if r := core.read(); r["code"] != "unknown_message" {
		t.Fatalf("unknown %v", r)
	}
}

func TestClientReconnectsAfterHelloError(t *testing.T) {
	sock, cores := startCore(t)
	h := &recordingHandler{said: make(chan string, 1), invoked: make(chan string, 1)}
	c := &Client{Socket: sock, InstanceID: "inst", AgentID: "agent", FinalDelivery: FinalDeliveryAutomatic, Handler: h}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	first := <-cores
	hello := first.read()
	first.write(Obj{"id": hello["id"], "m": "err", "code": "subject_unknown", "detail": nil})
	select {
	case second := <-cores:
		if second.read()["m"] != "hello" {
			t.Fatal("no hello after reconnect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not reconnect")
	}
	if c.Ready() {
		t.Fatal("ready without an accepted hello")
	}
}
