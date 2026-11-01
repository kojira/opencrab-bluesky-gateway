package gate

import (
	"bufio"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The digest of this declaration is pinned in opencrab gate-client client/tests.rs
// (s3_invoke_snapshot_rejects_stale_undeclared_and_mismatched_metadata).
func TestDeclarationDigestMatchesRustClient(t *testing.T) {
	ops := []any{Obj{
		"name":            "quasar.synthetic-v7",
		"description":     "synthetic",
		"input_schema":    Obj{"type": "object"},
		"output_schema":   nil,
		"callback_schema": nil,
		"authorization":   Obj{"allowed_callers": []any{"owner"}},
		"dispatch":        "background",
		"sub_engine":      "not_exposed",
		"sharing":         "agent_bound",
		"effect":          "state_change",
	}}
	got, err := DeclarationDigest(ops, FinalDeliveryAutomatic)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ae3998f8b460f71a2555ee1760ff82408162e4444a3a1d5dc5decac5020b889f"; got != want {
		t.Fatalf("digest %s, want %s", got, want)
	}
}

// hello_frame_with_capabilities_and_env (gate-client wire/hello_env.rs) without run_env.
func TestHelloFrameIsProtocol3(t *testing.T) {
	b, err := Marshal(HelloFrame("hello:i", "i", "a", nil, FinalDeliveryOperationDriven))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"agent_id":"a","final_delivery":"operation_driven","id":"hello:i","instance_id":"i","m":"hello","operation_protocol":1,"operations":[],"protocol":3}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
}

// said_frame_with_context: session key, origin, ref, author label; caller only when given.
func TestSaidFrameFields(t *testing.T) {
	f := SaidFrame("said:1", Said{
		Session: "bluesky-a", Origin: "at://did:plc:x/app.bsky.feed.post/1", Ref: "at://did:plc:x/app.bsky.feed.post/1",
		AuthorID: "did:plc:x", AuthorLabel: "Alice (@alice.test)", Text: "hi",
		Attachments: []Attachment{ImageURL("https://cdn.example/a.jpg")},
	})
	b, _ := Marshal(f)
	want := `{"attachments":[{"kind":"image","url":"https://cdn.example/a.jpg"}],"author_id":"did:plc:x","author_label":"Alice (@alice.test)","id":"said:1","m":"said","origin":"at://did:plc:x/app.bsky.feed.post/1","ref":"at://did:plc:x/app.bsky.feed.post/1","session":"bluesky-a","text":"hi"}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	f = SaidFrame("said:2", Said{Session: "s", Origin: "o", AuthorID: "a", Text: "t", Caller: CallerOwner})
	if f["caller"].(*Caller).Role != "owner" {
		t.Fatalf("caller %v", f["caller"])
	}
	if got, _ := Marshal(f); !strings.Contains(string(got), `"attachments":[]`) {
		t.Fatalf("attachments must be [] when empty: %s", got)
	}
}

// invoke_ok_frame / err_frame shapes.
func TestResponseFrames(t *testing.T) {
	cases := map[string]Obj{
		`{"id":"call-1","m":"ok","result":null}`:                         InvokeOkFrame("call-1", nil),
		`{"code":"operation_rejected","detail":null,"id":"c","m":"err"}`: ErrFrame("c", "operation_rejected"),
		`{"id":"s","m":"ok"}`:                                            OkFrame("s"),
	}
	for want, f := range cases {
		if got, _ := Marshal(f); string(got) != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
}

// The invoke frame of gate-client wire/tests.rs parse_invoke_ok.
func TestParseInvoke(t *testing.T) {
	raw := `{"id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","m":"invoke","invocation_protocol":1,"session":"room","declaration_digest":"` + strings.Repeat("a", 64) + `","operation":"reply","dispatch":"utterance","effect":"utterance","context":{"continuation_id":null},"payload":{"event":"e7","text":"hi"}}`
	m, err := ParseMsg([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.M != "invoke" || m.Session != "room" || m.Operation != "reply" || m.InvocationProtocol != 1 || m.Dispatch != "utterance" {
		t.Fatalf("%+v", m)
	}
	var p map[string]string
	_ = json.Unmarshal(m.Payload, &p)
	if p["text"] != "hi" {
		t.Fatalf("payload %s", m.Payload)
	}
}

func TestSayText(t *testing.T) {
	if text, _ := SayText(json.RawMessage(`{"text":"hi","extra":1}`)); text != "hi" {
		t.Fatal(text)
	}
	if text, _ := SayText(json.RawMessage(`{"text":""}`)); text != "" {
		t.Fatal(text)
	}
	if _, target := SayText(json.RawMessage(`{"text":"x","reply_target":"at://p"}`)); target != "at://p" {
		t.Fatal(target)
	}
}

func TestReadFrameLimit(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(`{"m":"ok","id":"x"}` + "\n" + strings.Repeat("a", MaxFrame+1) + "\n"))
	f, err := ReadFrame(r)
	if err != nil || string(f) != `{"m":"ok","id":"x"}` {
		t.Fatalf("%q %v", f, err)
	}
	if _, err := ReadFrame(r); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("want too large, got %v", err)
	}
}
