package bsky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// D-1123: chat calls carry the service proxy header; sendMessage gets the convo and text.
func TestSendDMGoesThroughTheChatProxy(t *testing.T) {
	var proxies, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nsid := strings.TrimPrefix(r.URL.Path, "/xrpc/")
		w.Header().Set("Content-Type", "application/json")
		switch nsid {
		case "com.atproto.server.createSession":
			_ = json.NewEncoder(w).Encode(map[string]string{"accessJwt": "a", "refreshJwt": "r", "did": "did:plc:bot"})
		case "chat.bsky.convo.sendMessage":
			proxies = append(proxies, r.Header.Get("atproto-proxy"))
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			data, _ := json.Marshal(b)
			bodies = append(bodies, string(data))
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "m1", "rev": "r1"})
		default:
			if r.Header.Get("atproto-proxy") != "" {
				t.Errorf("%s must not be proxied", nsid)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	c := &Client{Service: srv.URL, Handle: "bot.test", Password: "p"}
	ids, err := c.SendDM(context.Background(), "convo1", "やあ")
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if len(proxies) != 1 || proxies[0] != ChatProxy {
		t.Fatalf("proxy headers %v", proxies)
	}
	if bodies[0] != `{"convoId":"convo1","message":{"text":"やあ"}}` {
		t.Fatalf("body %s", bodies[0])
	}
	if _, err := c.ListNotifications(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

func TestDMEventMessageAndAuthor(t *testing.T) {
	var log DMLog
	_ = json.Unmarshal([]byte(`{"cursor":"c2","logs":[
		{"$type":"chat.bsky.convo.defs#logCreateMessage","convoId":"cv","rev":"r1","message":{"$type":"chat.bsky.convo.defs#messageView","id":"m1","rev":"r1","text":"hi","sender":{"did":"did:plc:k"},"sentAt":"2026-11-09T00:00:00Z"},"relatedProfiles":[{"did":"did:plc:k","handle":"kojira.io","displayName":"kojira"}]},
		{"$type":"chat.bsky.convo.defs#logCreateMessage","convoId":"cv","rev":"r2","message":{"$type":"chat.bsky.convo.defs#deletedMessageView","id":"m2","rev":"r2","sender":{"did":"did:plc:k"}}},
		{"$type":"chat.bsky.convo.defs#logReadMessage","convoId":"cv","rev":"r3"}]}`), &log)
	m, ok := log.Logs[0].Message()
	if !ok || m.Text != "hi" || log.Logs[0].Author("did:plc:k").Handle != "kojira.io" {
		t.Fatal(m, ok)
	}
	if _, ok := log.Logs[1].Message(); ok {
		t.Fatal("a deleted message is not a message")
	}
	if _, ok := log.Logs[2].Message(); ok {
		t.Fatal("a read event is not a message")
	}
}
