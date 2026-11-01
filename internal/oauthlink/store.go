// Package oauthlink links a bot to its Bluesky account with atproto OAuth (a confidential
// client: the signing key stays in the data dir, its public half is in the published client
// metadata). Tokens and DPoP keys live in oauth.json (mode 0600) and never leave this package's
// store except to the auth server and the PDS.
package oauthlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// pendingTTL is how long a started login may take before its request is dropped.
const pendingTTL = time.Hour

type pending struct {
	BotID   string                `json:"bot_id"`
	Created time.Time             `json:"created"`
	Request oauth.AuthRequestData `json:"request"`
}

// Link is the bot's OAuth session reference (the session itself is in Sessions).
type Link struct {
	DID       syntax.DID `json:"did"`
	SessionID string     `json:"session_id"`
}

type file struct {
	Pending  map[string]pending                 `json:"pending"`
	Sessions map[string]oauth.ClientSessionData `json:"sessions"`
	Bots     map[string]Link                    `json:"bots"`
}

// Store is oauth.json. It implements oauth.ClientAuthStore; pending requests also remember
// which bot started them.
type Store struct {
	path string
	mu   sync.Mutex
	f    file
}

var ErrNoSession = errors.New("no OAuth session")

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "oauth.json")}
	data, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.f); err != nil {
			return nil, fmt.Errorf("oauth store %s: %w", s.path, err)
		}
	}
	if s.f.Pending == nil {
		s.f.Pending = map[string]pending{}
	}
	if s.f.Sessions == nil {
		s.f.Sessions = map[string]oauth.ClientSessionData{}
	}
	if s.f.Bots == nil {
		s.f.Bots = map[string]Link{}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	for state, p := range s.f.Pending {
		if time.Since(p.Created) > pendingTTL {
			delete(s.f.Pending, state)
		}
	}
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".oauth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func sessionKey(did syntax.DID, sessionID string) string { return did.String() + "|" + sessionID }

func (s *Store) GetSession(_ context.Context, did syntax.DID, sessionID string) (*oauth.ClientSessionData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.f.Sessions[sessionKey(did, sessionID)]
	if !ok {
		return nil, ErrNoSession
	}
	return &d, nil
}

func (s *Store) SaveSession(_ context.Context, d oauth.ClientSessionData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Sessions[sessionKey(d.AccountDID, d.SessionID)] = d
	return s.saveLocked()
}

func (s *Store) DeleteSession(_ context.Context, did syntax.DID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.f.Sessions, sessionKey(did, sessionID))
	return s.saveLocked()
}

func (s *Store) GetAuthRequestInfo(_ context.Context, state string) (*oauth.AuthRequestData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.f.Pending[state]
	if !ok || time.Since(p.Created) > pendingTTL {
		return nil, errors.New("unknown or expired login request")
	}
	r := p.Request
	return &r, nil
}

// SaveAuthRequestInfo is not used directly (logins go through savePending, which also records
// the bot); it is here for oauth.ClientAuthStore.
func (s *Store) SaveAuthRequestInfo(_ context.Context, info oauth.AuthRequestData) error {
	return errors.New("use savePending")
}

func (s *Store) DeleteAuthRequestInfo(_ context.Context, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.f.Pending, state)
	return s.saveLocked()
}

func (s *Store) savePending(botID string, info oauth.AuthRequestData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.f.Pending[info.State]; dup {
		return errors.New("duplicate login state")
	}
	s.f.Pending[info.State] = pending{BotID: botID, Created: time.Now(), Request: info}
	return s.saveLocked()
}

func (s *Store) pendingBot(state string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.f.Pending[state]
	return p.BotID, ok
}

// Linked is the bot's session reference, if it has one.
func (s *Store) Linked(botID string) (Link, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.f.Bots[botID]
	return l, ok
}

func (s *Store) setLink(botID string, l Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.f.Bots[botID]; ok && old != l {
		delete(s.f.Sessions, sessionKey(old.DID, old.SessionID))
	}
	s.f.Bots[botID] = l
	return s.saveLocked()
}

// Unlink forgets the bot's session (tokens included).
func (s *Store) Unlink(botID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.f.Bots[botID]; ok {
		delete(s.f.Sessions, sessionKey(old.DID, old.SessionID))
	}
	delete(s.f.Bots, botID)
	return s.saveLocked()
}

// PathOf is the store's file.
func PathOf(s *Store) string { return s.path }
