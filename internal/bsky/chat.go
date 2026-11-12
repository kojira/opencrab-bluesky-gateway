package bsky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// DMMessage is one direct message as chat.bsky.convo.defs#messageView shows it.
type DMMessage struct {
	ID     string `json:"id"`
	Rev    string `json:"rev"`
	Text   string `json:"text"`
	Sender struct {
		DID string `json:"did"`
	} `json:"sender"`
	SentAt string `json:"sentAt"`
}

// DMEvent is one entry of chat.bsky.convo.getLog. Only logCreateMessage carries a message.
type DMEvent struct {
	Type    string          `json:"$type"`
	ConvoID string          `json:"convoId"`
	Rev     string          `json:"rev"`
	Raw     json.RawMessage `json:"message"`
	// RelatedProfiles are the profiles the message refers to (its sender among them).
	RelatedProfiles []Author `json:"relatedProfiles"`
}

// Author is the profile of did among the event's related profiles (did only when absent).
func (e DMEvent) Author(did string) Author {
	for _, a := range e.RelatedProfiles {
		if a.DID == did {
			return a
		}
	}
	return Author{DID: did}
}

// LogCreateMessage is the $type of a new message in the log.
const LogCreateMessage = "chat.bsky.convo.defs#logCreateMessage"

// Message is the event's message when it is a (not deleted) message view.
func (e DMEvent) Message() (DMMessage, bool) {
	var m struct {
		Type string `json:"$type"`
		DMMessage
	}
	if e.Type != LogCreateMessage || len(e.Raw) == 0 || json.Unmarshal(e.Raw, &m) != nil {
		return DMMessage{}, false
	}
	if m.Type != "" && m.Type != "chat.bsky.convo.defs#messageView" {
		return DMMessage{}, false
	}
	return m.DMMessage, m.ID != ""
}

// DMLog is one page of chat.bsky.convo.getLog.
type DMLog struct {
	Cursor string    `json:"cursor"`
	Logs   []DMEvent `json:"logs"`
}

// DMLog reads the account's chat event log after cursor (empty: the current position).
func (c *Client) DMLog(ctx context.Context, cursor string) (*DMLog, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var out DMLog
	if err := c.call(ctx, http.MethodGet, "chat.bsky.convo.getLog", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaxDMGraphemes is chat.bsky.convo.defs#messageInput's text limit.
const MaxDMGraphemes = 1000

// SendDM sends text to a conversation, split into messages within the limit. It returns the
// ids sent, so a partial send is visible to the caller.
func (c *Client) SendDM(ctx context.Context, convoID, text string) ([]string, error) {
	var ids []string
	for _, part := range SplitThread(text, MaxDMGraphemes) {
		var out DMMessage
		body := map[string]any{"convoId": convoID, "message": map[string]any{"text": part}}
		if err := c.call(ctx, http.MethodPost, "chat.bsky.convo.sendMessage", nil, body, &out); err != nil {
			return ids, err
		}
		ids = append(ids, out.ID)
	}
	return ids, nil
}

// ChatScope is the OAuth scope that lets the session use chat.bsky.* (DMs).
const ChatScope = "transition:chat.bsky"

// CanDM reports whether the session may read and send DMs: an OAuth session granted
// ChatScope. An app-password session is tried (its DM access is a flag of the app password).
func (c *Client) CanDM() bool {
	if c.OAuth == nil {
		return true
	}
	return HasChatScope(c.OAuth.Data.Scopes)
}

// HasChatScope reports whether granted scopes include ChatScope.
func HasChatScope(scopes []string) bool {
	for _, s := range scopes {
		if s == ChatScope {
			return true
		}
	}
	return false
}
