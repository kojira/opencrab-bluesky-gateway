package bsky

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// FeedPage is one page of posts from a feed or a search, with the cursor of the next page.
type FeedPage struct {
	Posts  []FeedItem
	Cursor string
}

// FeedItem is one post of a feed. RepostBy is set when it is in the feed because someone
// reposted it.
type FeedItem struct {
	Post     PostView
	RepostBy *Author
}

type feedViewPost struct {
	Post   PostView `json:"post"`
	Reason *struct {
		Type string `json:"$type"`
		By   Author `json:"by"`
	} `json:"reason"`
}

type feedResponse struct {
	Cursor string         `json:"cursor"`
	Feed   []feedViewPost `json:"feed"`
}

func (r feedResponse) page() *FeedPage {
	p := &FeedPage{Cursor: r.Cursor}
	for _, f := range r.Feed {
		item := FeedItem{Post: f.Post}
		if f.Reason != nil && strings.HasSuffix(f.Reason.Type, "#reasonRepost") {
			by := f.Reason.By
			item.RepostBy = &by
		}
		p.Posts = append(p.Posts, item)
	}
	return p
}

func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

// Timeline is the account's home timeline (app.bsky.feed.getTimeline).
func (c *Client) Timeline(ctx context.Context, limit int, cursor string) (*FeedPage, error) {
	var out feedResponse
	if err := c.call(ctx, http.MethodGet, "app.bsky.feed.getTimeline", pageQuery(limit, cursor), nil, &out); err != nil {
		return nil, err
	}
	return out.page(), nil
}

// AuthorFeed is one account's posts and reposts (app.bsky.feed.getAuthorFeed). actor is a handle
// (with or without @) or a did.
func (c *Client) AuthorFeed(ctx context.Context, actor string, limit int, cursor string) (*FeedPage, error) {
	q := pageQuery(limit, cursor)
	q.Set("actor", strings.TrimPrefix(strings.TrimSpace(actor), "@"))
	var out feedResponse
	if err := c.call(ctx, http.MethodGet, "app.bsky.feed.getAuthorFeed", q, nil, &out); err != nil {
		return nil, err
	}
	return out.page(), nil
}

// SearchPosts runs a post search (app.bsky.feed.searchPosts).
func (c *Client) SearchPosts(ctx context.Context, query string, limit int, cursor string) (*FeedPage, error) {
	q := pageQuery(limit, cursor)
	q.Set("q", query)
	var out struct {
		Cursor string     `json:"cursor"`
		Posts  []PostView `json:"posts"`
	}
	if err := c.call(ctx, http.MethodGet, "app.bsky.feed.searchPosts", q, nil, &out); err != nil {
		return nil, err
	}
	p := &FeedPage{Cursor: out.Cursor}
	for _, post := range out.Posts {
		p.Posts = append(p.Posts, FeedItem{Post: post})
	}
	return p, nil
}

// ThreadPost is one post of a thread: Depth 0 is the requested post, negative depths are its
// parents (-1 the direct parent), positive depths are replies.
type ThreadPost struct {
	Post  PostView
	Depth int
}

type threadNode struct {
	Type    string          `json:"$type"`
	Post    *PostView       `json:"post"`
	Parent  json.RawMessage `json:"parent"`
	Replies []threadNode    `json:"replies"`
}

// Thread is a post with its parents and replies (app.bsky.feed.getPostThread), parents first.
func (c *Client) Thread(ctx context.Context, uri string, depth int) ([]ThreadPost, error) {
	q := url.Values{"uri": {uri}, "depth": {fmt.Sprint(depth)}, "parentHeight": {"10"}}
	var out struct {
		Thread threadNode `json:"thread"`
	}
	if err := c.call(ctx, http.MethodGet, "app.bsky.feed.getPostThread", q, nil, &out); err != nil {
		return nil, err
	}
	if out.Thread.Post == nil {
		return nil, &APIError{Status: 404, Code: "NotFound", Message: "post not found"}
	}
	var parents []ThreadPost
	raw := out.Thread.Parent
	for d := -1; len(raw) > 0; d-- {
		var n threadNode
		if json.Unmarshal(raw, &n) != nil || n.Post == nil {
			break
		}
		parents = append([]ThreadPost{{Post: *n.Post, Depth: d}}, parents...)
		raw = n.Parent
	}
	posts := append(parents, ThreadPost{Post: *out.Thread.Post})
	var walk func(nodes []threadNode, d int)
	walk = func(nodes []threadNode, d int) {
		for _, n := range nodes {
			if n.Post == nil {
				continue
			}
			posts = append(posts, ThreadPost{Post: *n.Post, Depth: d})
			walk(n.Replies, d+1)
		}
	}
	walk(out.Thread.Replies, 1)
	return posts, nil
}

// PostURI turns a post reference into its at:// uri: an at:// uri as it is, or a bsky.app post URL
// (https://bsky.app/profile/<handle or did>/post/<rkey>), resolving the handle.
func (c *Client) PostURI(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "at://") {
		return ref, nil
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Host != "bsky.app" && u.Host != "www.bsky.app") {
		return "", fmt.Errorf("bsky: not a post: %s", ref)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "profile" || parts[2] != "post" || parts[1] == "" || parts[3] == "" {
		return "", fmt.Errorf("bsky: not a post URL: %s", ref)
	}
	did := parts[1]
	if !strings.HasPrefix(did, "did:") {
		if did, err = c.ResolveHandle(ctx, did); err != nil {
			return "", err
		}
	}
	return "at://" + did + "/app.bsky.feed.post/" + parts[3], nil
}

// GetPost is one post by its at:// uri (app.bsky.feed.getPosts); NotFound when it is gone.
func (c *Client) GetPost(ctx context.Context, uri string) (PostView, error) {
	posts, err := c.GetPosts(ctx, []string{uri})
	if err != nil {
		return PostView{}, err
	}
	if len(posts) == 0 {
		return PostView{}, &APIError{Status: 404, Code: "NotFound", Message: "post not found"}
	}
	return posts[0], nil
}
