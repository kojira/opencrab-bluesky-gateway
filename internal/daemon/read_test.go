package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
)

// feedStub is an XRPC server for the read operations. It records each request's query and
// answers with canned feeds; pages are served by cursor.
type feedStub struct {
	mu      sync.Mutex
	queries map[string]url.Values
}

func post(uri, handle, name, text string, likes int) map[string]any {
	return map[string]any{
		"uri": uri, "cid": "c", "author": map[string]any{"did": "did:plc:" + handle, "handle": handle + ".bsky.social", "displayName": name},
		"record":     map[string]any{"$type": "app.bsky.feed.post", "text": text, "createdAt": "2026-11-06T01:02:03.000Z"},
		"replyCount": 1, "repostCount": 2, "likeCount": likes,
	}
}

func (s *feedStub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	nsid := strings.TrimPrefix(r.URL.Path, "/xrpc/")
	s.queries[nsid] = r.URL.Query()
	s.mu.Unlock()
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	q := r.URL.Query()
	switch nsid {
	case "com.atproto.server.createSession":
		reply(map[string]string{"accessJwt": "t", "refreshJwt": "r", "did": "did:plc:bot", "handle": "bot.test"})
	case "app.bsky.feed.getTimeline":
		if q.Get("cursor") == "" {
			reply(map[string]any{"cursor": "page2", "feed": []any{
				map[string]any{"post": post("at://did:plc:alice/app.bsky.feed.post/1", "alice", "Alice", "おはよう", 3)},
				map[string]any{"post": post("at://did:plc:bob/app.bsky.feed.post/2", "bob", "", "rt me", 0),
					"reason": map[string]any{"$type": "app.bsky.feed.defs#reasonRepost", "by": map[string]any{"did": "did:plc:carol", "handle": "carol.bsky.social"}}},
			}})
			return
		}
		reply(map[string]any{"feed": []any{map[string]any{"post": post("at://did:plc:alice/app.bsky.feed.post/0", "alice", "Alice", "older", 0)}}})
	case "app.bsky.feed.getAuthorFeed":
		reply(map[string]any{"cursor": "a2", "feed": []any{map[string]any{"post": post("at://did:plc:alice/app.bsky.feed.post/9", "alice", "Alice", strings.Repeat("長", 200), 1)}}})
	case "app.bsky.feed.searchPosts":
		reply(map[string]any{"cursor": "s2", "posts": []any{post("at://did:plc:dan/app.bsky.feed.post/5", "dan", "Dan", "OpenCrab いいね", 7)}})
	case "app.bsky.feed.getPostThread":
		reply(map[string]any{"thread": map[string]any{
			"$type":  "app.bsky.feed.defs#threadViewPost",
			"post":   post("at://did:plc:bob/app.bsky.feed.post/b", "bob", "Bob", strings.Repeat("全", 200), 0),
			"parent": map[string]any{"$type": "app.bsky.feed.defs#threadViewPost", "post": post("at://did:plc:alice/app.bsky.feed.post/a", "alice", "Alice", "root", 0)},
			"replies": []any{map[string]any{"$type": "app.bsky.feed.defs#threadViewPost", "post": post("at://did:plc:carol/app.bsky.feed.post/c", "carol", "", "reply", 0),
				"replies": []any{map[string]any{"$type": "app.bsky.feed.defs#threadViewPost", "post": post("at://did:plc:dan/app.bsky.feed.post/d", "dan", "", "deeper", 0)}}}},
		}})
	case "app.bsky.feed.getPosts":
		reply(map[string]any{"posts": []any{post(q.Get("uris"), "alice", "Alice", "x", 0)}})
	case "com.atproto.repo.createRecord":
		reply(map[string]string{"uri": "at://did:plc:bot/x/1", "cid": "c"})
	default:
		w.WriteHeader(404)
	}
}

func newReadOps(t *testing.T) (*feedStub, *Ops) {
	s := &feedStub{queries: map[string]url.Values{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	return s, &Ops{Bsky: &bsky.Client{Service: srv.URL, Handle: "bot.test", Password: "p"}}
}

func invokeRead(t *testing.T, o *Ops, op, payload string) gate.Obj {
	t.Helper()
	res, out := o.Invoke(context.Background(), "s", op, json.RawMessage(payload))
	if out != gate.OutcomeOk {
		t.Fatalf("%s: outcome %v", op, out)
	}
	return res.(gate.Obj)
}

func TestReadTimelineRendersPostsAndPagesWithCursor(t *testing.T) {
	s, o := newReadOps(t)
	res := invokeRead(t, o, "read_timeline", `{}`)
	if got := s.queries["app.bsky.feed.getTimeline"]; got.Get("limit") != fmt.Sprint(DefaultReadLimit) || got.Get("cursor") != "" {
		t.Fatalf("query %v", got)
	}
	text := res["posts"].(string)
	for _, want := range []string{
		"Alice (@alice.bsky.social) 2026-11-06 01:02 ↩1 🔁2 ♥3\nおはよう\nat://did:plc:alice/app.bsky.feed.post/1",
		"🔁 @carol.bsky.social がリポスト\n@bob.bsky.social",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	if res["next_cursor"] != "page2" {
		t.Fatalf("cursor %v", res["next_cursor"])
	}
	res = invokeRead(t, o, "read_timeline", `{"cursor":"page2","limit":500}`)
	if got := s.queries["app.bsky.feed.getTimeline"]; got.Get("cursor") != "page2" || got.Get("limit") != fmt.Sprint(MaxReadLimit) {
		t.Fatalf("paged query %v", got)
	}
	if !strings.Contains(res["posts"].(string), "older") || res["next_cursor"] != "" {
		t.Fatalf("second page %v", res)
	}
}

func TestReadAuthorFeedTakesHandleAndCutsLongText(t *testing.T) {
	s, o := newReadOps(t)
	res := invokeRead(t, o, "read_author_feed", `{"user":"@alice.bsky.social","limit":5}`)
	if got := s.queries["app.bsky.feed.getAuthorFeed"]; got.Get("actor") != "alice.bsky.social" || got.Get("limit") != "5" {
		t.Fatalf("query %v", got)
	}
	text := res["posts"].(string)
	if !strings.Contains(text, strings.Repeat("長", PostTextShown)+"…（続きは read_thread）") || strings.Contains(text, strings.Repeat("長", PostTextShown+1)) {
		t.Fatalf("long text not cut:\n%s", text)
	}
	if res["next_cursor"] != "a2" {
		t.Fatal(res["next_cursor"])
	}
	if _, out := o.Invoke(context.Background(), "s", "read_author_feed", json.RawMessage(`{}`)); out != gate.OutcomeRejected {
		t.Fatal("missing user accepted")
	}
}

func TestSearchPostsSendsTheQuery(t *testing.T) {
	s, o := newReadOps(t)
	res := invokeRead(t, o, "search_posts", `{"query":"OpenCrab","cursor":"s1"}`)
	if got := s.queries["app.bsky.feed.searchPosts"]; got.Get("q") != "OpenCrab" || got.Get("cursor") != "s1" {
		t.Fatalf("query %v", got)
	}
	if !strings.Contains(res["posts"].(string), "Dan (@dan.bsky.social)") || res["next_cursor"] != "s2" {
		t.Fatalf("result %v", res)
	}
}

func TestReadThreadShowsParentsThePostAndRepliesInFull(t *testing.T) {
	s, o := newReadOps(t)
	res := invokeRead(t, o, "read_thread", `{"event":"at://did:plc:bob/app.bsky.feed.post/b"}`)
	if got := s.queries["app.bsky.feed.getPostThread"]; got.Get("uri") != "at://did:plc:bob/app.bsky.feed.post/b" || got.Get("depth") != fmt.Sprint(DefaultDepth) {
		t.Fatalf("query %v", got)
	}
	text := res["posts"].(string)
	root := strings.Index(text, "root")
	focus := strings.Index(text, "▶ Bob")
	r1 := strings.Index(text, "  @carol.bsky.social")
	r2 := strings.Index(text, "    @dan.bsky.social")
	if !(root >= 0 && root < focus && focus < r1 && r1 < r2) {
		t.Fatalf("order/indent wrong:\n%s", text)
	}
	if !strings.Contains(text, strings.Repeat("全", 200)) {
		t.Fatal("thread text must be whole")
	}
	if _, out := o.Invoke(context.Background(), "s", "read_thread", json.RawMessage(`{"event":"e3"}`)); out != gate.OutcomeRejected {
		t.Fatal("unresolved short ref accepted")
	}
}

func TestLikeAndRepostTakeAPostURIFromAReadResult(t *testing.T) {
	s, o := newReadOps(t)
	uri := "at://did:plc:alice/app.bsky.feed.post/1"
	for _, op := range []string{"like", "repost"} {
		if _, out := o.Invoke(context.Background(), "s", op, json.RawMessage(`{"event":"`+uri+`"}`)); out != gate.OutcomeOk {
			t.Fatalf("%s: %v", op, out)
		}
		if s.queries["app.bsky.feed.getPosts"].Get("uris") != uri {
			t.Fatalf("%s did not look up the uri", op)
		}
	}
}

// estimateTokens is a conservative stand-in for core's o200k count: one token per non-ASCII
// character (Japanese text is usually less), one per 4 ASCII bytes.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return other + ascii/4
}

func TestDefaultPageFitsCoreToolResultSize(t *testing.T) {
	// The default page of the longest posts a listing shows (every text at the cut), with core's
	// result wrapper headroom: under 2,000 of core's 2,500-token tool-result size.
	var items []bsky.FeedItem
	for i := 0; i < DefaultReadLimit; i++ {
		var p bsky.PostView
		_ = json.Unmarshal([]byte(`{"uri":"at://did:plc:aaaaaaaaaaaaaaaaaaaaaaaa/app.bsky.feed.post/3mzly7f3mp22d","author":{"handle":"someone.bsky.social","displayName":"長い表示名のアカウント"},"record":{"text":"`+strings.Repeat("あ", 300)+`","createdAt":"2026-11-06T01:02:03Z"}}`), &p)
		items = append(items, bsky.FeedItem{Post: p})
	}
	out, _ := json.Marshal(gate.Obj{"posts": RenderFeed(items), "next_cursor": "3mzly7f3mp22d::bafyreidpm7sfb7ljpsjgtxxf6n2zti2l5mc7lzk67jqx"})
	if n := estimateTokens(string(out)); n > 2_000 {
		t.Fatalf("default page is about %d tokens; it would not fit core's tool-result size", n)
	}
}
