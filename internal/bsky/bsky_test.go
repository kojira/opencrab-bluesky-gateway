package bsky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stub is a tiny XRPC server: sessions, getPosts, resolveHandle, createRecord, uploadBlob.
type stub struct {
	mu      sync.Mutex
	posts   map[string]PostView
	created []map[string]any
	logins  int
	expired bool
}

func newStub(t *testing.T) (*stub, *Client) {
	s := &stub{posts: map[string]PostView{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, &Client{Service: srv.URL, Handle: "bot.test", Password: "app-pass"}
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nsid := strings.TrimPrefix(r.URL.Path, "/xrpc/")
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	if nsid != "com.atproto.server.createSession" && nsid != "com.atproto.server.refreshSession" {
		if s.expired && r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(400)
			reply(map[string]string{"error": "ExpiredToken", "message": "expired"})
			return
		}
	}
	switch nsid {
	case "com.atproto.server.createSession":
		s.logins++
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "app-pass" {
			w.WriteHeader(401)
			reply(map[string]string{"error": "AuthenticationRequired", "message": "Invalid identifier or password"})
			return
		}
		tok := "new"
		if s.logins == 1 && s.expired {
			tok = "old"
		}
		reply(map[string]string{"accessJwt": tok, "refreshJwt": "r", "did": "did:plc:bot", "handle": "bot.test"})
	case "com.atproto.server.refreshSession":
		reply(map[string]string{"accessJwt": "new", "refreshJwt": "r2", "did": "did:plc:bot"})
	case "app.bsky.feed.getPosts":
		var out []PostView
		for _, u := range r.URL.Query()["uris"] {
			if p, ok := s.posts[u]; ok {
				out = append(out, p)
			}
		}
		reply(map[string]any{"posts": out})
	case "com.atproto.identity.resolveHandle":
		if r.URL.Query().Get("handle") == "alice.test" {
			reply(map[string]string{"did": "did:plc:alice"})
			return
		}
		w.WriteHeader(400)
		reply(map[string]string{"error": "InvalidRequest", "message": "Unable to resolve handle"})
	case "com.atproto.repo.createRecord":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.created = append(s.created, body)
		n := len(s.created)
		reply(map[string]string{"uri": "at://did:plc:bot/" + body["collection"].(string) + "/" + string(rune('a'+n-1)), "cid": "cid" + string(rune('a'+n-1))})
	case "com.atproto.repo.uploadBlob":
		reply(map[string]any{"blob": map[string]any{"$type": "blob", "ref": map[string]string{"$link": "bafyimg"}, "mimeType": r.Header.Get("Content-Type"), "size": r.ContentLength}})
	default:
		w.WriteHeader(404)
	}
}

func record(t *testing.T, s *stub, i int) map[string]any {
	t.Helper()
	return s.created[i]["record"].(map[string]any)
}

func TestReplyToAReplyUsesTheThreadRoot(t *testing.T) {
	s, c := newStub(t)
	root := "at://did:plc:alice/app.bsky.feed.post/root"
	child := "at://did:plc:alice/app.bsky.feed.post/child"
	s.posts[root] = PostView{URI: root, CID: "cidroot", Record: json.RawMessage(`{"text":"root"}`)}
	s.posts[child] = PostView{URI: child, CID: "cidchild", Record: json.RawMessage(`{"text":"child","reply":{"root":{"uri":"` + root + `","cid":"cidroot"},"parent":{"uri":"` + root + `","cid":"cidroot"}}}`)}

	if _, err := c.Post(context.Background(), "answer", child, nil); err != nil {
		t.Fatal(err)
	}
	reply := record(t, s, 0)["reply"].(map[string]any)
	if reply["root"].(map[string]any)["uri"] != root || reply["parent"].(map[string]any)["uri"] != child {
		t.Fatalf("reply ref %v", reply)
	}
	// replying to a top-level post: root and parent are that post
	if _, err := c.Post(context.Background(), "answer", root, nil); err != nil {
		t.Fatal(err)
	}
	reply = record(t, s, 1)["reply"].(map[string]any)
	if reply["root"].(map[string]any)["uri"] != root || reply["parent"].(map[string]any)["uri"] != root {
		t.Fatalf("reply ref %v", reply)
	}
}

func TestLongPostBecomesAThread(t *testing.T) {
	s, c := newStub(t)
	text := strings.Repeat("あいうえお。", 70) // 420 graphemes
	uris, err := c.Post(context.Background(), text, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(uris) != 2 || len(s.created) != 2 {
		t.Fatalf("posts %d", len(s.created))
	}
	first, second := record(t, s, 0), record(t, s, 1)
	if first["reply"] != nil {
		t.Fatal("first part must not be a reply")
	}
	r := second["reply"].(map[string]any)
	if r["parent"].(map[string]any)["uri"] != uris[0] || r["root"].(map[string]any)["uri"] != uris[0] {
		t.Fatalf("second part reply %v", r)
	}
	if !strings.HasSuffix(first["text"].(string), "。") || Graphemes(first["text"].(string)) > MaxGraphemes {
		t.Fatalf("first part %q", first["text"])
	}
	if first["text"].(string)+second["text"].(string) != text {
		t.Fatal("text lost in split")
	}
}

func TestSplitThreadKeepsLinksWhole(t *testing.T) {
	url := "https://example.com/" + strings.Repeat("x", 40)
	text := strings.Repeat("あ", 280) + url
	parts := SplitThread(text, MaxGraphemes)
	if len(parts) != 2 || parts[1] != url {
		t.Fatalf("parts %q", parts)
	}
	for _, p := range parts {
		if Graphemes(p) > MaxGraphemes {
			t.Fatalf("too long: %d", Graphemes(p))
		}
	}
	// emoji with modifiers count as one grapheme
	if n := Graphemes("👍🏽👨‍👩‍👧"); n != 2 {
		t.Fatalf("graphemes %d", n)
	}
}

func TestFacetsMarkLinksAndResolvedMentions(t *testing.T) {
	_, c := newStub(t)
	text := "見て https://example.com/a. @alice.test と @nobody.test へ"
	fs := Facets(context.Background(), text, c.ResolveHandle)
	if len(fs) != 2 {
		t.Fatalf("facets %+v", fs)
	}
	link := fs[0]
	if got := text[link.Index.ByteStart:link.Index.ByteEnd]; got != "https://example.com/a" {
		t.Fatalf("link %q", got)
	}
	if link.Features[0].(LinkFeature).URI != "https://example.com/a" {
		t.Fatal("link uri")
	}
	m := fs[1]
	if got := text[m.Index.ByteStart:m.Index.ByteEnd]; got != "@alice.test" {
		t.Fatalf("mention %q", got)
	}
	if m.Features[0].(MentionFeature).DID != "did:plc:alice" {
		t.Fatal("mention did")
	}
}

func TestExpiredTokenIsRefreshedOnce(t *testing.T) {
	s, c := newStub(t)
	s.expired = true
	p := "at://did:plc:alice/app.bsky.feed.post/p"
	s.posts[p] = PostView{URI: p, CID: "c"}
	if _, err := c.Like(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(s.created) != 1 || s.created[0]["collection"] != "app.bsky.feed.like" {
		t.Fatalf("created %v", s.created)
	}
}

func TestImagesAreUploadedAndEmbedded(t *testing.T) {
	s, c := newStub(t)
	_, err := c.Post(context.Background(), "photo", "", []Image{{Data: []byte("jpeg"), MimeType: "image/jpeg", Alt: "a.jpg"}})
	if err != nil {
		t.Fatal(err)
	}
	embed := record(t, s, 0)["embed"].(map[string]any)
	if embed["$type"] != "app.bsky.embed.images" {
		t.Fatalf("embed %v", embed)
	}
	img := embed["images"].([]any)[0].(map[string]any)
	if img["image"].(map[string]any)["ref"].(map[string]any)["$link"] != "bafyimg" {
		t.Fatalf("image %v", img)
	}
}

func TestWrongPasswordIsPermanent(t *testing.T) {
	_, c := newStub(t)
	c.Password = "wrong"
	err := c.Login(context.Background())
	if err == nil {
		t.Fatal("login with a wrong password")
	}
}
