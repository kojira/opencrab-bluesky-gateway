// Package store keeps the gateway's bots in one JSON file (mode 0600) under the data dir.
package store

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kojira/opencrab-bluesky-gateway/internal/inbound"
)

const (
	DefaultPollSecs = 15
	MinPollSecs     = 10
	MaxPollSecs     = 3600
)

// Bot is one agent's Bluesky account.
type Bot struct {
	InstanceID string `json:"instance_id"`
	AgentID    string `json:"agent_id"`
	Label      string `json:"label"`
	Handle     string `json:"handle"`
	// AppPassword is write-only: kept here, never logged, never returned by the API.
	AppPassword string   `json:"app_password"`
	Service     string   `json:"service,omitempty"`
	Enabled     bool     `json:"enabled"`
	PollSecs    int      `json:"poll_secs"`
	Owners      []string `json:"owners"`
}

// Configured reports whether the bot can log in.
func (b Bot) Configured() bool { return b.Handle != "" && b.AppPassword != "" }

type file struct {
	Bots    []Bot                     `json:"bots"`
	Cursors map[string]inbound.Cursor `json:"cursors"`
}

// Store is the bots file. All methods are safe for concurrent use.
type Store struct {
	path string
	mu   sync.Mutex
	f    file
}

var ErrNotFound = errors.New("bot not found")
var ErrExists = errors.New("this agent already has a bot")

// Open loads (or starts) the store at dataDir/bots.json.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "bots.json"), f: file{Cursors: map[string]inbound.Cursor{}}}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.f); err != nil {
		return nil, fmt.Errorf("store %s: %w", s.path, err)
	}
	if s.f.Cursors == nil {
		s.f.Cursors = map[string]inbound.Cursor{}
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".bots-*.json")
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

// InstanceID is the bot's gate instance: a UUIDv5 of the agent, so the same agent keeps the
// same instance (and core session) after the bot is deleted and added again.
func InstanceID(agentID string) string {
	ns := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8} // NameSpace_URL
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte("opencrab-bluesky-gateway:" + agentID))
	u := h.Sum(nil)[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

func (s *Store) List() []Bot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Bot(nil), s.f.Bots...)
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func (s *Store) Get(id string) (Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.f.Bots {
		if b.InstanceID == id {
			return b, nil
		}
	}
	return Bot{}, ErrNotFound
}

// Create adds a disabled bot without credentials for an agent.
func (s *Store) Create(agentID, label string) (Bot, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return Bot{}, errors.New("agent_id is required")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = agentID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.f.Bots {
		if b.AgentID == agentID {
			return Bot{}, ErrExists
		}
	}
	b := Bot{InstanceID: InstanceID(agentID), AgentID: agentID, Label: label, Enabled: true, PollSecs: DefaultPollSecs, Owners: []string{}}
	s.f.Bots = append(s.f.Bots, b)
	return b, s.saveLocked()
}

// Edit is a partial update; nil fields are unchanged. An empty AppPassword keeps the old one.
type Edit struct {
	Label       *string   `json:"label"`
	Handle      *string   `json:"handle"`
	AppPassword *string   `json:"app_password"`
	Service     *string   `json:"service"`
	Enabled     *bool     `json:"enabled"`
	PollSecs    *int      `json:"poll_secs"`
	Owners      *[]string `json:"owners"`
}

func (s *Store) Update(id string, e Edit) (Bot, error) {
	if e.PollSecs != nil && (*e.PollSecs < MinPollSecs || *e.PollSecs > MaxPollSecs) {
		return Bot{}, fmt.Errorf("poll_secs must be %d..%d", MinPollSecs, MaxPollSecs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.f.Bots {
		b := &s.f.Bots[i]
		if b.InstanceID != id {
			continue
		}
		if e.Label != nil && strings.TrimSpace(*e.Label) != "" {
			b.Label = strings.TrimSpace(*e.Label)
		}
		if e.Handle != nil {
			h := strings.TrimPrefix(strings.TrimSpace(*e.Handle), "@")
			if h != b.Handle {
				delete(s.f.Cursors, b.InstanceID) // another account starts from now
			}
			b.Handle = h
		}
		if e.AppPassword != nil && strings.TrimSpace(*e.AppPassword) != "" {
			b.AppPassword = strings.TrimSpace(*e.AppPassword)
		}
		if e.Service != nil {
			b.Service = strings.TrimSpace(*e.Service)
		}
		if e.Enabled != nil {
			b.Enabled = *e.Enabled
		}
		if e.PollSecs != nil {
			b.PollSecs = *e.PollSecs
		}
		if e.Owners != nil {
			owners := []string{}
			for _, o := range *e.Owners {
				if o = strings.TrimSpace(o); o != "" {
					owners = append(owners, o)
				}
			}
			b.Owners = owners
		}
		return *b, s.saveLocked()
	}
	return Bot{}, ErrNotFound
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.f.Bots {
		if b.InstanceID == id {
			s.f.Bots = append(s.f.Bots[:i], s.f.Bots[i+1:]...)
			delete(s.f.Cursors, id)
			return s.saveLocked()
		}
	}
	return ErrNotFound
}

func (s *Store) Cursor(id string) inbound.Cursor {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.f.Cursors[id]
	c.Seen = append([]string(nil), c.Seen...)
	return c
}

func (s *Store) SetCursor(id string, c inbound.Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Cursors[id] = c
	return s.saveLocked()
}

// PathOf is the store's file.
func PathOf(s *Store) string { return s.path }
