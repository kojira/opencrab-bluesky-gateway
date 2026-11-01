// Package bsky is the small part of the AT Protocol XRPC API this gateway uses.
package bsky

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultService = "https://bsky.social"

// APIError is an XRPC error answer.
type APIError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("bsky: %d %s: %s", e.Status, e.Code, e.Message)
}

// Permanent reports whether retrying the same request cannot succeed (4xx other than 429/401).
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != 429 && e.Status != 401
}

// Client is one account's session. The app password stays in memory only.
type Client struct {
	Service  string
	Handle   string
	Password string
	HTTP     *http.Client

	mu      sync.Mutex
	access  string
	refresh string
	DID     string
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) service() string {
	if c.Service != "" {
		return strings.TrimRight(c.Service, "/")
	}
	return DefaultService
}

type session struct {
	AccessJwt  string `json:"accessJwt"`
	RefreshJwt string `json:"refreshJwt"`
	DID        string `json:"did"`
	Handle     string `json:"handle"`
}

// Login creates a session with the handle and app password.
func (c *Client) Login(ctx context.Context) error {
	var s session
	err := c.do(ctx, http.MethodPost, "com.atproto.server.createSession", nil,
		map[string]string{"identifier": c.Handle, "password": c.Password}, "", &s)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.access, c.refresh, c.DID = s.AccessJwt, s.RefreshJwt, s.DID
	if s.Handle != "" {
		c.Handle = s.Handle
	}
	c.mu.Unlock()
	return nil
}

func (c *Client) refreshSession(ctx context.Context) error {
	c.mu.Lock()
	refresh := c.refresh
	c.mu.Unlock()
	if refresh == "" {
		return c.Login(ctx)
	}
	var s session
	if err := c.do(ctx, http.MethodPost, "com.atproto.server.refreshSession", nil, nil, refresh, &s); err != nil {
		return c.Login(ctx)
	}
	c.mu.Lock()
	c.access, c.refresh = s.AccessJwt, s.RefreshJwt
	c.mu.Unlock()
	return nil
}

// call is an authenticated XRPC call; an expired token is refreshed once.
func (c *Client) call(ctx context.Context, method, nsid string, query url.Values, body any, out any) error {
	c.mu.Lock()
	token := c.access
	c.mu.Unlock()
	if token == "" {
		if err := c.Login(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		token = c.access
		c.mu.Unlock()
	}
	err := c.do(ctx, method, nsid, query, body, token, out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Code == "ExpiredToken") {
		if err := c.refreshSession(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		token = c.access
		c.mu.Unlock()
		return c.do(ctx, method, nsid, query, body, token, out)
	}
	return err
}

func (c *Client) do(ctx context.Context, method, nsid string, query url.Values, body any, token string, out any) error {
	u := c.service() + "/xrpc/" + nsid
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	contentType := ""
	switch b := body.(type) {
	case nil:
	case rawBody:
		rd = bytes.NewReader(b.data)
		contentType = b.contentType
	default:
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(data, e)
		return e
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type rawBody struct {
	data        []byte
	contentType string
}

// Author is a post's author as the API shows it.
type Author struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
}

// Label is "displayName (@handle)", or "@handle".
func (a Author) Label() string {
	if strings.TrimSpace(a.DisplayName) != "" {
		return a.DisplayName + " (@" + a.Handle + ")"
	}
	return "@" + a.Handle
}

// Notification is one item of listNotifications.
type Notification struct {
	URI       string          `json:"uri"`
	CID       string          `json:"cid"`
	Author    Author          `json:"author"`
	Reason    string          `json:"reason"`
	Record    json.RawMessage `json:"record"`
	IndexedAt string          `json:"indexedAt"`
}

type NotificationPage struct {
	Cursor        string         `json:"cursor"`
	Notifications []Notification `json:"notifications"`
}

func (c *Client) ListNotifications(ctx context.Context, limit int) (*NotificationPage, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	var page NotificationPage
	if err := c.call(ctx, http.MethodGet, "app.bsky.notification.listNotifications", q, nil, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// StrongRef points at one version of a record.
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// PostRecord is the app.bsky.feed.post record fields this gateway reads and writes.
type PostRecord struct {
	Type      string          `json:"$type"`
	Text      string          `json:"text"`
	CreatedAt string          `json:"createdAt"`
	Facets    []Facet         `json:"facets,omitempty"`
	Reply     *ReplyRef       `json:"reply,omitempty"`
	Embed     json.RawMessage `json:"embed,omitempty"`
	Langs     []string        `json:"langs,omitempty"`
}

type ReplyRef struct {
	Root   StrongRef `json:"root"`
	Parent StrongRef `json:"parent"`
}

// PostView is a post from getPosts.
type PostView struct {
	URI    string          `json:"uri"`
	CID    string          `json:"cid"`
	Author Author          `json:"author"`
	Record json.RawMessage `json:"record"`
	Embed  json.RawMessage `json:"embed"`
}

func (c *Client) GetPosts(ctx context.Context, uris []string) ([]PostView, error) {
	q := url.Values{}
	for _, u := range uris {
		q.Add("uris", u)
	}
	var out struct {
		Posts []PostView `json:"posts"`
	}
	if err := c.call(ctx, http.MethodGet, "app.bsky.feed.getPosts", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Posts, nil
}

// CreateRecord writes a record into the account's repo.
func (c *Client) CreateRecord(ctx context.Context, collection string, record any) (StrongRef, error) {
	c.mu.Lock()
	did := c.DID
	c.mu.Unlock()
	if did == "" {
		if err := c.Login(ctx); err != nil {
			return StrongRef{}, err
		}
		c.mu.Lock()
		did = c.DID
		c.mu.Unlock()
	}
	var ref StrongRef
	err := c.call(ctx, http.MethodPost, "com.atproto.repo.createRecord", nil,
		map[string]any{"repo": did, "collection": collection, "record": record}, &ref)
	return ref, err
}

// Blob is an uploaded blob reference, kept as the API returned it.
type Blob = json.RawMessage

func (c *Client) UploadBlob(ctx context.Context, data []byte, mimeType string) (Blob, error) {
	var out struct {
		Blob json.RawMessage `json:"blob"`
	}
	if err := c.call(ctx, http.MethodPost, "com.atproto.repo.uploadBlob", nil, rawBody{data, mimeType}, &out); err != nil {
		return nil, err
	}
	return out.Blob, nil
}

// ResolveHandle turns a handle into a DID.
func (c *Client) ResolveHandle(ctx context.Context, handle string) (string, error) {
	var out struct {
		DID string `json:"did"`
	}
	err := c.call(ctx, http.MethodGet, "com.atproto.identity.resolveHandle", url.Values{"handle": {handle}}, nil, &out)
	return out.DID, err
}

// SelfDID is the logged-in account's DID (logging in when needed).
func (c *Client) SelfDID(ctx context.Context) (string, error) {
	c.mu.Lock()
	did := c.DID
	c.mu.Unlock()
	if did != "" {
		return did, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.DID, nil
}

// LoggedIn reports whether a session exists.
func (c *Client) LoggedIn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.access != ""
}
