package oauthlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Scopes are requested for every login: atproto plus app-password-level access.
var Scopes = []string{"atproto", "transition:generic"}

// Config is the "oauth" block of the gateway config.
type Config struct {
	// ClientID is the public URL of the client metadata document (see ClientMetadata).
	ClientID string `json:"client_id"`
	// CallbackURL is this gateway's /oauth/callback as the browser reaches it (https).
	CallbackURL string `json:"callback_url"`
}

// Linker runs the login flow and hands out bots' sessions.
type Linker struct {
	App   *oauth.ClientApp
	Store *Store
	// AuthServer finds the auth server of a PDS (tests replace it; nil resolves over the web).
	AuthServer func(ctx context.Context, pdsURL string) (*oauth.AuthServerMetadata, error)
}

const keyFile = "oauth-client-key"

// New loads the client signing key from dataDir (creating it, mode 0600, on first use).
func New(cfg Config, dataDir string, st *Store) (*Linker, error) {
	if cfg.ClientID == "" || cfg.CallbackURL == "" {
		return nil, errors.New("oauth needs client_id and callback_url")
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, keyFile))
	if err != nil {
		return nil, err
	}
	pub, err := key.PublicKey()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(pub.Bytes())
	cc := oauth.NewPublicConfig(cfg.ClientID, cfg.CallbackURL, Scopes)
	cc.UserAgent = "opencrab-bluesky-gateway"
	if err := cc.SetClientSecret(key, hex.EncodeToString(sum[:6])); err != nil {
		return nil, err
	}
	return &Linker{App: oauth.NewClientApp(&cc, st), Store: st}, nil
}

func loadOrCreateKey(path string) (*atcrypto.PrivateKeyP256, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		k, err := atcrypto.ParsePrivateMultibase(strings.TrimSpace(string(data)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		p, ok := k.(*atcrypto.PrivateKeyP256)
		if !ok {
			return nil, fmt.Errorf("%s: not a P-256 key", path)
		}
		return p, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	k, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(k.Multibase() + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return k, f.Close()
}

// ClientMetadata is the document to publish at ClientID (public key only).
func (l *Linker) ClientMetadata() oauth.ClientMetadata {
	m := l.App.Config.ClientMetadata()
	jwks := l.App.Config.PublicJWKS()
	for i := range jwks.Keys {
		jwks.Keys[i].Use = "sig"
	}
	m.JWKS = &jwks
	name := "OpenCrab Bluesky gateway"
	m.ClientName = &name
	return m
}

func (l *Linker) authServer(ctx context.Context, pds string) (*oauth.AuthServerMetadata, error) {
	if l.AuthServer != nil {
		return l.AuthServer(ctx, pds)
	}
	u, err := l.App.Resolver.ResolveAuthServerURL(ctx, pds)
	if err != nil {
		return nil, fmt.Errorf("resolving auth server: %w", err)
	}
	return l.App.Resolver.ResolveAuthServerMetadata(ctx, u)
}

// Start sends the authorization request (PAR) for the bot's account and returns the URL to send
// the browser to.
func (l *Linker) Start(ctx context.Context, botID, handle string) (string, error) {
	id, err := syntax.ParseAtIdentifier(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	if err != nil {
		return "", fmt.Errorf("not a handle or DID: %q", handle)
	}
	ident, err := l.App.Dir.Lookup(ctx, id)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", handle, err)
	}
	pds := ident.PDSEndpoint()
	if pds == "" {
		return "", fmt.Errorf("%s has no PDS", handle)
	}
	meta, err := l.authServer(ctx, pds)
	if err != nil {
		return "", err
	}
	info, err := l.App.SendAuthRequest(ctx, meta, l.App.Config.Scopes, id.String())
	if err != nil {
		return "", err
	}
	did := ident.DID
	info.AccountDID = &did
	if err := l.Store.savePending(botID, *info); err != nil {
		return "", err
	}
	q := url.Values{"client_id": {l.App.Config.ClientID}, "request_uri": {info.RequestURI}}
	return meta.AuthorizationEndpoint + "?" + q.Encode(), nil
}

// Callback finishes a login: checks state, iss, PKCE (via the token request) and that the
// account is the one the login started with, then links the session to the bot.
func (l *Linker) Callback(ctx context.Context, params url.Values) (string, syntax.DID, error) {
	botID, ok := l.Store.pendingBot(params.Get("state"))
	if !ok {
		return "", "", errors.New("unknown or expired login request")
	}
	sess, err := l.App.ProcessCallback(ctx, params)
	if err != nil {
		_ = l.Store.DeleteAuthRequestInfo(ctx, params.Get("state"))
		return botID, "", err
	}
	if err := l.Store.setLink(botID, Link{DID: sess.AccountDID, SessionID: sess.SessionID}); err != nil {
		return botID, "", err
	}
	return botID, sess.AccountDID, nil
}

// Session is the bot's OAuth session, or ErrNoSession.
func (l *Linker) Session(ctx context.Context, botID string) (*oauth.ClientSession, error) {
	link, ok := l.Store.Linked(botID)
	if !ok {
		return nil, ErrNoSession
	}
	return l.App.ResumeSession(ctx, link.DID, link.SessionID)
}

// Logout revokes the bot's tokens (when the server supports it) and forgets the session.
func (l *Linker) Logout(ctx context.Context, botID string) error {
	if link, ok := l.Store.Linked(botID); ok {
		if sess, err := l.App.ResumeSession(ctx, link.DID, link.SessionID); err == nil && sess.Data.AuthServerRevocationEndpoint != "" {
			_ = sess.RevokeSession(ctx)
		}
	}
	return l.Store.Unlink(botID)
}
