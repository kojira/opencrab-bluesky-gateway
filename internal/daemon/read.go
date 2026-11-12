package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
	"github.com/kojira/opencrab-bluesky-gateway/internal/gate"
)

// Reader is the read side of the Bluesky client (D-1118).
type Reader interface {
	Timeline(ctx context.Context, limit int, cursor string) (*bsky.FeedPage, error)
	AuthorFeed(ctx context.Context, actor string, limit int, cursor string) (*bsky.FeedPage, error)
	SearchPosts(ctx context.Context, query string, limit int, cursor string) (*bsky.FeedPage, error)
	Thread(ctx context.Context, uri string, depth int) ([]bsky.ThreadPost, error)
	PostURI(ctx context.Context, ref string) (string, error)
	GetPost(ctx context.Context, uri string) (bsky.PostView, error)
}

const (
	// DefaultReadLimit keeps one page within core's tool-result size (about 2,500 tokens for a
	// gateway operation): each post's text is cut to PostTextShown graphemes.
	DefaultReadLimit = 8
	MaxReadLimit     = 100
	DefaultDepth     = 6
	MaxDepth         = 10
	// PostTextShown is how much of a post's text a listing shows; read_thread shows it whole.
	PostTextShown = 150
)

// readSize is the size note shared by the read operations' descriptions.
const readSize = "結果は同じターンで返る。投稿ごとに 作者・時刻(UTC)・返信/リポスト/いいね数・本文・画像の https URL・at:// の uri を返し、結果の images に全画像の URL を並べる。uri は reply / like / repost の event にそのまま渡せる。1 回の結果がおよそ 2,500 トークンを超えると、本文は会話に入らずファイルに保存され、その場所だけが返る（ws_read で読める）。既定の limit は収まる目安。続きは結果の next_cursor を cursor に渡して読む。"

func readOperations(decl func(name, desc string, input gate.Obj, dispatch, effect, sharing string) gate.Obj) []gate.Obj {
	str := func(desc string) gate.Obj { return gate.Obj{"type": "string", "description": desc} }
	ref := func(desc string) gate.Obj {
		return gate.Obj{"type": "string", "description": desc, "format": "short-ref"}
	}
	num := func(desc string) gate.Obj { return gate.Obj{"type": "integer", "description": desc} }
	limit := num(fmt.Sprintf("件数（既定 %d、最大 %d）", DefaultReadLimit, MaxReadLimit))
	cursor := str("前の結果の next_cursor（続きを読むとき）")
	return []gate.Obj{
		decl("resolve", "e番号の投稿の詳細を取得する（会話で省略された全文や、投稿の画像・返信先・引用先を確かめる）。ref に会話の e番号、at:// の uri、または bsky.app の投稿 URL（https://bsky.app/profile/…/post/…）。結果は同じターンで返る: 作者・時刻(UTC)・返信/リポスト/いいね数・本文（全文）・返信先(parent)とスレッドの最初(root)の uri・引用先の uri・画像の https URL（images）。",
			gate.Obj{"type": "object", "required": []any{"ref"}, "properties": gate.Obj{
				"ref": ref("投稿（e番号、at:// の uri、bsky.app の投稿 URL）"),
			}}, "inline", "read_only", "conversation_bound"),
		decl("read_author_feed", "ある人の投稿（リポストを含む）を新しい順に読む。user に会話の u番号、handle（例 alice.bsky.social）か did。"+readSize,
			gate.Obj{"type": "object", "required": []any{"user"}, "properties": gate.Obj{
				"user": ref("読む相手（u番号、handle、did）"), "limit": limit, "cursor": cursor,
			}}, "inline", "read_only", "agent_bound"),
		decl("read_thread", "投稿のスレッド（親の投稿と返信）を読む。event に会話の e番号か at:// の uri。本文は省略せずに返す。"+readSize,
			gate.Obj{"type": "object", "required": []any{"event"}, "properties": gate.Obj{
				"event": ref("読む投稿（e番号か at:// の uri）"),
				"depth": num(fmt.Sprintf("返信をたどる深さ（既定 %d、最大 %d）", DefaultDepth, MaxDepth)),
			}}, "inline", "read_only", "conversation_bound"),
		decl("read_timeline", "自分のホームタイムライン（フォロー中の人の投稿とリポスト）を新しい順に読む。"+readSize,
			gate.Obj{"type": "object", "properties": gate.Obj{"limit": limit, "cursor": cursor}},
			"inline", "read_only", "agent_bound"),
		decl("search_posts", "Bluesky 全体から投稿を検索する。query に検索語。"+readSize,
			gate.Obj{"type": "object", "required": []any{"query"}, "properties": gate.Obj{
				"query": str("検索語"), "limit": limit, "cursor": cursor,
			}}, "inline", "read_only", "agent_bound"),
	}
}

type readParams struct {
	Ref    string `json:"ref"`
	User   string `json:"user"`
	Query  string `json:"query"`
	Event  string `json:"event"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
	Depth  int    `json:"depth"`
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// read runs a read operation. ok is false when the operation is not a read.
func (o *Ops) read(ctx context.Context, operation string, raw json.RawMessage) (any, gate.Outcome, bool) {
	var p readParams
	if json.Unmarshal(raw, &p) != nil {
		return nil, gate.OutcomeRejected, true
	}
	limit := clamp(p.Limit, DefaultReadLimit, MaxReadLimit)
	var page *bsky.FeedPage
	var err error
	switch operation {
	case "read_timeline":
		page, err = o.Bsky.Timeline(ctx, limit, p.Cursor)
	case "read_author_feed":
		if strings.TrimSpace(p.User) == "" {
			return nil, gate.OutcomeRejected, true
		}
		page, err = o.Bsky.AuthorFeed(ctx, p.User, limit, p.Cursor)
	case "search_posts":
		if strings.TrimSpace(p.Query) == "" {
			return nil, gate.OutcomeRejected, true
		}
		page, err = o.Bsky.SearchPosts(ctx, p.Query, limit, p.Cursor)
	case "resolve":
		return o.resolve(ctx, p.Ref)
	case "read_thread":
		if !isPostURI(p.Event) {
			return nil, gate.OutcomeRejected, true
		}
		posts, err := o.Bsky.Thread(ctx, p.Event, clamp(p.Depth, DefaultDepth, MaxDepth))
		if err != nil {
			return nil, outcomeOf(err), true
		}
		text, images := RenderThread(posts)
		return gate.Obj{"posts": text, "images": images}, gate.OutcomeOk, true
	default:
		return nil, 0, false
	}
	if err != nil {
		return nil, outcomeOf(err), true
	}
	text, images := RenderFeed(page.Posts)
	return gate.Obj{"posts": text, "images": images, "next_cursor": page.Cursor}, gate.OutcomeOk, true
}

type postText struct {
	Text      string          `json:"text"`
	CreatedAt string          `json:"createdAt"`
	Reply     json.RawMessage `json:"reply"`
	Embed     json.RawMessage `json:"embed"`
}

// embedType is the $type of a post record's embed ("" when it has none).
func embedType(embed json.RawMessage) string {
	var e struct {
		Type string `json:"$type"`
	}
	_ = json.Unmarshal(embed, &e)
	return e.Type
}

func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// renderPost is one post: a header line, the text, a line per image URL, and the uri. full keeps
// the whole text. It also gives the post's image URLs (bsky.ImageURLs, the one place for them).
func renderPost(p bsky.PostView, indent string, full bool) (string, []string) {
	var rec postText
	_ = json.Unmarshal(p.Record, &rec)
	when := rec.CreatedAt
	if when == "" {
		when = p.IndexedAt
	}
	images := bsky.ImageURLs(p.Author.DID, rec.Embed)
	var marks []string
	if len(rec.Reply) > 0 && string(rec.Reply) != "null" {
		marks = append(marks, "返信")
	}
	if len(images) > 0 {
		marks = append(marks, fmt.Sprintf("画像%d枚", len(images)))
	}
	if strings.Contains(embedType(rec.Embed), "embed.record") {
		marks = append(marks, "引用あり")
	}
	head := fmt.Sprintf("%s%s %s ↩%d 🔁%d ♥%d", indent, p.Author.Label(), shortTime(when), p.ReplyCount, p.RepostCount, p.LikeCount)
	if len(marks) > 0 {
		head += " [" + strings.Join(marks, "・") + "]"
	}
	text := strings.TrimSpace(rec.Text)
	if !full && bsky.Graphemes(text) > PostTextShown {
		text = bsky.CutGraphemes(text, PostTextShown) + "…（続きは read_thread）"
	}
	text = strings.ReplaceAll(text, "\n", "\n"+indent)
	out := head + "\n" + indent + text
	for _, u := range images {
		out += "\n" + indent + "🖼 " + u
	}
	return out + "\n" + indent + p.URI, images
}

// RenderFeed is the text of a page of posts, newest first as the API returns them, and the image
// URLs of all of them.
func RenderFeed(items []bsky.FeedItem) (string, []string) {
	images := []string{}
	if len(items) == 0 {
		return "（投稿はありません）", images
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		s, imgs := renderPost(it.Post, "", false)
		images = append(images, imgs...)
		if it.RepostBy != nil {
			s = "🔁 " + it.RepostBy.Label() + " がリポスト\n" + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n"), images
}

// RenderThread is a thread: parents, the requested post (marked ▶), then replies indented by
// depth. Texts are whole. It also gives the image URLs of all the posts.
func RenderThread(posts []bsky.ThreadPost) (string, []string) {
	images := []string{}
	parts := make([]string, 0, len(posts))
	for _, tp := range posts {
		indent := ""
		if tp.Depth > 0 {
			indent = strings.Repeat("  ", tp.Depth)
		}
		s, imgs := renderPost(tp.Post, indent, true)
		images = append(images, imgs...)
		if tp.Depth == 0 {
			s = "▶ " + s
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n"), images
}

// resolve is one post in full: the rendered post plus its reply parent and root, the post it
// quotes, and its image URLs (the same rendering and image URLs as read_*).
func (o *Ops) resolve(ctx context.Context, ref string) (any, gate.Outcome, bool) {
	if strings.TrimSpace(ref) == "" {
		return nil, gate.OutcomeRejected, true
	}
	uri, err := o.Bsky.PostURI(ctx, ref)
	if err != nil {
		var apiErr *bsky.APIError
		if errors.As(err, &apiErr) {
			return nil, outcomeOf(err), true
		}
		return nil, gate.OutcomeRejected, true
	}
	post, err := o.Bsky.GetPost(ctx, uri)
	if err != nil {
		return nil, outcomeOf(err), true
	}
	text, images := renderPost(post, "", true)
	var rec struct {
		Reply *bsky.ReplyRef  `json:"reply"`
		Embed json.RawMessage `json:"embed"`
	}
	_ = json.Unmarshal(post.Record, &rec)
	out := gate.Obj{"post": text, "uri": post.URI, "images": images}
	if rec.Reply != nil {
		out["reply_parent"] = rec.Reply.Parent.URI
		out["reply_root"] = rec.Reply.Root.URI
	}
	if q := quotedURI(rec.Embed); q != "" {
		out["quote"] = q
	}
	return out, gate.OutcomeOk, true
}

// quotedURI is the uri of the post a record embeds (app.bsky.embed.record, or the record of
// app.bsky.embed.recordWithMedia), or "".
func quotedURI(embed json.RawMessage) string {
	var e struct {
		Record json.RawMessage `json:"record"`
	}
	if len(embed) == 0 || json.Unmarshal(embed, &e) != nil || len(e.Record) == 0 {
		return ""
	}
	var ref struct {
		URI    string `json:"uri"`
		Record *struct {
			URI string `json:"uri"`
		} `json:"record"`
	}
	if json.Unmarshal(e.Record, &ref) != nil {
		return ""
	}
	if ref.URI != "" {
		return ref.URI
	}
	if ref.Record != nil {
		return ref.Record.URI
	}
	return ""
}
