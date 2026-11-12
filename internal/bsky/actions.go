package bsky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Image is one image to attach to a post.
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
	posts, err := c.GetPosts(ctx, []string{uri})
	if err != nil {
		return nil, err
	}
	if len(posts) == 0 {
		return nil, &APIError{Status: 404, Code: "NotFound", Message: "post not found"}
	}
	parent := StrongRef{URI: posts[0].URI, CID: posts[0].CID}
	var rec struct {
		Reply *ReplyRef `json:"reply"`
	}
	_ = json.Unmarshal(posts[0].Record, &rec)
	root := parent
	if rec.Reply != nil && rec.Reply.Root.URI != "" {
		root = rec.Reply.Root
	}
	return &ReplyRef{Root: root, Parent: parent}, nil
}

// Post publishes text (split into a thread when longer than one post) with images on the first
// part. replyTo is an at:// post uri to answer, or "". It returns the uris of the posts made.
func (c *Client) Post(ctx context.Context, text, replyTo string, images []Image) ([]string, error) {
	parts := SplitThread(text, MaxGraphemes)
	if len(parts) == 0 {
		if len(images) == 0 {
			return nil, errors.New("bsky: nothing to post")
		}
		parts = []string{""}
	}
	if len(images) > MaxImages {
		return nil, fmt.Errorf("bsky: at most %d images", MaxImages)
	}
	var reply *ReplyRef
	if replyTo != "" {
		r, err := c.ReplyRefFor(ctx, replyTo)
		if err != nil {
			return nil, err
		}
		reply = r
	}
	var embed json.RawMessage
	if len(images) > 0 {
		var items []map[string]any
		for _, img := range images {
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
		embed, _ = json.Marshal(map[string]any{"$type": "app.bsky.embed.images", "images": items})
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
	posts, err := c.GetPosts(ctx, []string{uri})
	if err != nil {
		return StrongRef{}, err
	}
	if len(posts) == 0 {
		return StrongRef{}, &APIError{Status: 404, Code: "NotFound", Message: "post not found"}
	}
	return StrongRef{URI: posts[0].URI, CID: posts[0].CID}, nil
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
