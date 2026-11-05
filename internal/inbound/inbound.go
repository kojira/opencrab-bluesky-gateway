// Package inbound turns Bluesky notifications into core saids and remembers what was delivered.
package inbound

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
)

// SessionKey is the core session of a bot: one conversation per agent, like Nostr.
func SessionKey(agentID string) string { return "bluesky-" + agentID }

// Reasons delivered to the agent.
var reasons = map[string]bool{"mention": true, "reply": true, "quote": true}

// ReplyContext is what the model is told with every delivered mention/reply/quote (D-1117): how
// an answer reaches the author on Bluesky. A fact, not an instruction; the model decides.
const ReplyContext = "これは Bluesky であなた宛ての投稿（メンション/返信/引用）です。相手のスレッドで答えるには reply（event にこの投稿の e番号）を使います。地の文で書いた本文は、この投稿とは関係のない新しい投稿として公開されます。"

type postRecord struct {
	Text  string          `json:"text"`
	Reply *bsky.ReplyRef  `json:"reply"`
	Embed json.RawMessage `json:"embed"`
}

type blobRef struct {
	Ref struct {
		Link string `json:"$link"`
	} `json:"ref"`
	MimeType string `json:"mimeType"`
}

type imagesEmbed struct {
	Type   string `json:"$type"`
	Images []struct {
		Image blobRef `json:"image"`
	} `json:"images"`
	Media *imagesEmbed `json:"media"`
}

// ImageURLs gives the CDN URLs of a post record's images (images or recordWithMedia).
func ImageURLs(authorDID string, embed json.RawMessage) []string {
	if len(embed) == 0 {
		return nil
	}
	var e imagesEmbed
	if json.Unmarshal(embed, &e) != nil {
		return nil
	}
	imgs := e.Images
	if e.Media != nil {
		imgs = e.Media.Images
	}
	var out []string
	for _, img := range imgs {
		if img.Image.Ref.Link == "" {
			continue
		}
		out = append(out, "https://cdn.bsky.app/img/feed_fullsize/plain/"+authorDID+"/"+img.Image.Ref.Link+"@jpeg")
	}
	return out
}

// Map turns one notification into a said for the bot's session. ok is false for notifications
// that are not delivered: other reasons, the bot's own posts, and empty posts.
func Map(n bsky.Notification, selfDID, agentID string, owners []string) (gate.Said, bool) {
	if !reasons[n.Reason] || n.Author.DID == "" || n.Author.DID == selfDID || n.URI == "" {
		return gate.Said{}, false
	}
	var rec postRecord
	_ = json.Unmarshal(n.Record, &rec)
	s := gate.Said{
		Session:       SessionKey(agentID),
		Origin:        n.URI,
		Ref:           n.URI,
		AuthorID:      n.Author.DID,
		AuthorLabel:   truncate(n.Author.Label(), 100),
		Text:          rec.Text,
		SystemContext: ReplyContext,
	}
	for _, u := range ImageURLs(n.Author.DID, rec.Embed) {
		s.Attachments = append(s.Attachments, gate.ImageURL(u))
	}
	if rec.Reply != nil && rec.Reply.Parent.URI != "" {
		s.ReplyTarget = rec.Reply.Parent.URI
	}
	if IsOwner(n.Author, owners) {
		s.Caller = gate.CallerOwner
	}
	if strings.TrimSpace(s.Text) == "" && len(s.Attachments) == 0 {
		return gate.Said{}, false
	}
	return s, true
}

// IsOwner matches the author against the owner list (DIDs or handles, with or without @).
func IsOwner(a bsky.Author, owners []string) bool {
	for _, o := range owners {
		o = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(o), "@"))
		if o != "" && (o == strings.ToLower(a.DID) || o == strings.ToLower(a.Handle)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// MaxSeen bounds the remembered uris.
const MaxSeen = 500

// Cursor is what a bot has delivered: the newest notification time and the recent uris.
type Cursor struct {
	Since string   `json:"since"`
	Seen  []string `json:"seen"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Pending gives the notifications not delivered yet, oldest first. A cursor without Since
// (first run) delivers nothing: the bot starts from now instead of answering old mentions.
func (c *Cursor) Pending(ns []bsky.Notification) []bsky.Notification {
	if c.Since == "" {
		return nil
	}
	since := parseTime(c.Since)
	seen := make(map[string]bool, len(c.Seen))
	for _, u := range c.Seen {
		seen[u] = true
	}
	var out []bsky.Notification
	for _, n := range ns {
		if seen[n.URI] || parseTime(n.IndexedAt).Before(since) {
			continue
		}
		out = append(out, n)
	}
	sort.SliceStable(out, func(i, j int) bool { return parseTime(out[i].IndexedAt).Before(parseTime(out[j].IndexedAt)) })
	return out
}

// Start sets the cursor to the newest of ns (or now) when it has none.
func (c *Cursor) Start(ns []bsky.Notification) {
	if c.Since != "" {
		return
	}
	newest := time.Now().UTC()
	for _, n := range ns {
		if t := parseTime(n.IndexedAt); t.After(newest) {
			newest = t
		}
		c.Seen = append(c.Seen, n.URI)
	}
	c.Since = newest.Format(time.RFC3339Nano)
	if len(c.Seen) > MaxSeen {
		c.Seen = c.Seen[len(c.Seen)-MaxSeen:]
	}
}

// Done records one handled notification.
func (c *Cursor) Done(n bsky.Notification) {
	if t := parseTime(n.IndexedAt); t.After(parseTime(c.Since)) {
		c.Since = n.IndexedAt
	}
	c.Seen = append(c.Seen, n.URI)
	if len(c.Seen) > MaxSeen {
		c.Seen = c.Seen[len(c.Seen)-MaxSeen:]
	}
}
