package inbound

import (
	"encoding/json"
	"testing"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
)

func note(uri, did, reason, at, record string) bsky.Notification {
	return bsky.Notification{URI: uri, Author: bsky.Author{DID: did, Handle: "alice.test", DisplayName: "Alice"}, Reason: reason, IndexedAt: at, Record: json.RawMessage(record)}
}

func TestMentionBecomesSaidWithRefAndImages(t *testing.T) {
	n := note("at://did:plc:alice/app.bsky.feed.post/1", "did:plc:alice", "reply", "2026-11-01T00:00:01Z",
		`{"text":"こんにちは","reply":{"root":{"uri":"at://r","cid":"c"},"parent":{"uri":"at://p","cid":"c"}},
		  "embed":{"$type":"app.bsky.embed.images","images":[{"image":{"ref":{"$link":"bafk1"},"mimeType":"image/png"}}]}}`)
	s, ok := Map(n, "did:plc:bot", "agent-1", nil)
	if !ok {
		t.Fatal("not mapped")
	}
	if s.Session != "bluesky-agent-1" || s.Origin != n.URI || s.Ref != n.URI || s.AuthorID != "did:plc:alice" {
		t.Fatalf("%+v", s)
	}
	if s.AuthorLabel != "Alice (@alice.test)" || s.Text != "こんにちは" || s.ReplyTarget != "at://p" || s.Caller != nil {
		t.Fatalf("%+v", s)
	}
	if len(s.Attachments) != 1 || s.Attachments[0].URL != "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:alice/bafk1@jpeg" {
		t.Fatalf("attachments %+v", s.Attachments)
	}
}

func TestOwnPostsLikesAndFollowsAreNotDelivered(t *testing.T) {
	rec := `{"text":"x"}`
	cases := []bsky.Notification{
		note("at://1", "did:plc:bot", "mention", "2026-11-01T00:00:01Z", rec), // the bot itself
		note("at://2", "did:plc:alice", "like", "2026-11-01T00:00:01Z", rec),
		note("at://3", "did:plc:alice", "follow", "2026-11-01T00:00:01Z", rec),
		note("at://4", "did:plc:alice", "repost", "2026-11-01T00:00:01Z", rec),
		note("at://5", "did:plc:alice", "mention", "2026-11-01T00:00:01Z", `{"text":"  "}`),
	}
	for _, n := range cases {
		if _, ok := Map(n, "did:plc:bot", "a", nil); ok {
			t.Errorf("%s (%s) was delivered", n.URI, n.Reason)
		}
	}
	if _, ok := Map(note("at://6", "did:plc:alice", "quote", "2026-11-01T00:00:01Z", rec), "did:plc:bot", "a", nil); !ok {
		t.Error("quote not delivered")
	}
}

func TestOwnerByHandleOrDID(t *testing.T) {
	n := note("at://1", "did:plc:alice", "mention", "2026-11-01T00:00:01Z", `{"text":"hi"}`)
	for _, owners := range [][]string{{"@Alice.test"}, {"did:plc:alice"}} {
		s, _ := Map(n, "did:plc:bot", "a", owners)
		if s.Caller == nil || s.Caller.Role != "owner" {
			t.Errorf("owners %v: caller %v", owners, s.Caller)
		}
	}
}

func TestCursorStartsFromNowAndNeverRepeats(t *testing.T) {
	old := note("at://old", "did:plc:alice", "mention", "2026-11-01T00:00:00Z", `{"text":"old"}`)
	var c Cursor
	if got := c.Pending([]bsky.Notification{old}); len(got) != 0 {
		t.Fatal("first run must not deliver old mentions")
	}
	c.Start([]bsky.Notification{old})
	if c.Pending([]bsky.Notification{old}) != nil {
		t.Fatal("old mention delivered after start")
	}
	// two new ones, listed newest first like the API
	since := c.Since
	a := note("at://a", "did:plc:alice", "mention", "2099-01-01T00:00:01Z", `{"text":"a"}`)
	b := note("at://b", "did:plc:alice", "mention", "2099-01-01T00:00:02Z", `{"text":"b"}`)
	got := c.Pending([]bsky.Notification{b, a, old})
	if len(got) != 2 || got[0].URI != "at://a" || got[1].URI != "at://b" {
		t.Fatalf("pending %v (since %s)", got, since)
	}
	c.Done(got[0])
	// a restart after handling only "a": "b" is still pending, "a" is not
	again := c.Pending([]bsky.Notification{b, a, old})
	if len(again) != 1 || again[0].URI != "at://b" {
		t.Fatalf("after restart %v", again)
	}
	// same indexedAt but a different uri is still delivered (dedupe is by uri)
	c.Done(again[0])
	twin := note("at://b2", "did:plc:alice", "mention", "2099-01-01T00:00:02Z", `{"text":"b2"}`)
	if got := c.Pending([]bsky.Notification{twin, b}); len(got) != 1 || got[0].URI != "at://b2" {
		t.Fatalf("twin %v", got)
	}
}
