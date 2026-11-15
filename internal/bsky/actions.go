package bsky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Image is one file to attach to a post: an image, or an mp4 video (MimeType tells which).
type Image struct {
	Data     []byte
	MimeType string
	Alt      string
}

// MaxImages is Bluesky's image limit per post.
const MaxImages = 4

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// ReplyRefFor gives the reply reference of an answer to the post at uri: the parent is that post,
// the root is its thread root (the post itself when it is not a reply).
func (c *Client) ReplyRefFor(ctx context.Context, uri string) (*ReplyRef, error) {
	post, err := c.GetPost(ctx, uri)
	if err != nil {
		return nil, err
	}
	parent := StrongRef{URI: post.URI, CID: post.CID}
	var rec struct {
		Reply *ReplyRef `json:"reply"`
	}
	_ = json.Unmarshal(post.Record, &rec)
	root := parent
	if rec.Reply != nil && rec.Reply.Root.URI != "" {
		root = rec.Reply.Root
	}
	return &ReplyRef{Root: root, Parent: parent}, nil
}

// Post publishes text (split into a thread when longer than one post) with media on the first
// part: up to MaxImages images, or exactly one mp4 video. replyTo is an at:// post uri to answer,
// or "". It returns the uris of the posts made.
func (c *Client) Post(ctx context.Context, text, replyTo string, media []Image) ([]string, error) {
	parts := SplitThread(text, MaxGraphemes)
	if len(parts) == 0 {
		if len(media) == 0 {
			return nil, errors.New("bsky: nothing to post")
		}
		parts = []string{""}
	}
	if err := CheckMedia(media); err != nil {
		return nil, err
	}
	var reply *ReplyRef
	if replyTo != "" {
		r, err := c.ReplyRefFor(ctx, replyTo)
		if err != nil {
			return nil, err
		}
		reply = r
	}
	embed, err := c.mediaEmbed(ctx, media)
	if err != nil {
		return nil, err
	}
	var uris []string
	for i, part := range parts {
		rec := PostRecord{
			Type:      "app.bsky.feed.post",
			Text:      part,
			CreatedAt: now(),
			Facets:    Facets(ctx, part, c.ResolveHandle),
			Reply:     reply,
		}
		if i == 0 {
			rec.Embed = embed
		}
		ref, err := c.CreateRecord(ctx, "app.bsky.feed.post", rec)
		if err != nil {
			return uris, err
		}
		uris = append(uris, ref.URI)
		// the next part answers this one, in the same thread
		root := ref
		if reply != nil {
			root = reply.Root
		}
		reply = &ReplyRef{Root: root, Parent: ref}
	}
	return uris, nil
}

func (c *Client) subjectRef(ctx context.Context, uri string) (StrongRef, error) {
	post, err := c.GetPost(ctx, uri)
	if err != nil {
		return StrongRef{}, err
	}
	return StrongRef{URI: post.URI, CID: post.CID}, nil
}

func (c *Client) Like(ctx context.Context, uri string) (string, error) {
	s, err := c.subjectRef(ctx, uri)
	if err != nil {
		return "", err
	}
	ref, err := c.CreateRecord(ctx, "app.bsky.feed.like", map[string]any{"$type": "app.bsky.feed.like", "subject": s, "createdAt": now()})
	return ref.URI, err
}

func (c *Client) Repost(ctx context.Context, uri string) (string, error) {
	s, err := c.subjectRef(ctx, uri)
	if err != nil {
		return "", err
	}
	ref, err := c.CreateRecord(ctx, "app.bsky.feed.repost", map[string]any{"$type": "app.bsky.feed.repost", "subject": s, "createdAt": now()})
	return ref.URI, err
}

// Follow follows a handle (with or without @) or a did.
func (c *Client) Follow(ctx context.Context, who string) (string, error) {
	who = strings.TrimPrefix(strings.TrimSpace(who), "@")
	did := who
	if !strings.HasPrefix(who, "did:") {
		d, err := c.ResolveHandle(ctx, who)
		if err != nil {
			return "", err
		}
		did = d
	}
	ref, err := c.CreateRecord(ctx, "app.bsky.graph.follow", map[string]any{"$type": "app.bsky.graph.follow", "subject": did, "createdAt": now()})
	return ref.URI, err
}

// CheckMedia is the one rule for what a post can carry: up to MaxImages images, or one mp4 video
// alone (within the video limits). It answers before anything is uploaded.
func CheckMedia(media []Image) error {
	videos := 0
	for _, m := range media {
		if IsVideo(m.MimeType) {
			videos++
		}
	}
	switch {
	case videos > 0 && len(media) > 1:
		return &MediaError{"1つの投稿に付けられるのは、画像（4枚まで）か動画1本のどちらかです。動画と画像、または動画2本以上は一緒に付けられません"}
	case videos == 1:
		_, err := CheckVideo(media[0])
		return err
	case len(media) > MaxImages:
		return &MediaError{fmt.Sprintf("画像は1つの投稿に%d枚までです", MaxImages)}
	}
	return nil
}

// mediaEmbed uploads the media and builds the post's embed (images or video), or nil.
func (c *Client) mediaEmbed(ctx context.Context, media []Image) (json.RawMessage, error) {
	if len(media) == 0 {
		return nil, nil
	}
	if IsVideo(media[0].MimeType) {
		v := media[0]
		info, err := CheckVideo(v)
		if err != nil {
			return nil, err
		}
		blob, err := c.UploadVideo(ctx, v)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{
			"$type":       "app.bsky.embed.video",
			"video":       blob,
			"alt":         v.Alt,
			"aspectRatio": map[string]int{"width": info.Width, "height": info.Height},
		})
	}
	var items []map[string]any
	for _, img := range media {
		img, w, h, err := PrepareImage(img)
		if err != nil {
			return nil, err
		}
		blob, err := c.UploadBlob(ctx, img.Data, img.MimeType)
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"alt":         img.Alt,
			"image":       blob,
			"aspectRatio": map[string]int{"width": w, "height": h},
		})
	}
	return json.Marshal(map[string]any{"$type": "app.bsky.embed.images", "images": items})
}
