package oauthlink

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/kojira/opencrab-bluesky-gateway/internal/bsky"
)

const botDID = "did:plc:testbot0000000000000000"

// stubServer is an auth server and PDS in one (like a self-hosted PDS): PAR with a mandatory
// DPoP nonce, a token endpoint that checks PKCE, and an XRPC endpoint that wants DPoP.
type stubServer struct {
	*httptest.Server
	mu        sync.Mutex
	challenge string
	par       url.Values
	subject   string
	xrpcAuth  []string
}

func newStub(t *testing.T) *stubServer {
	s := &stubServer{subject: botDID}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /par", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("DPoP") == "" {
			w.WriteHeader(400)
			return
		}
		if !strings.Contains(dpopPayload(r.Header.Get("DPoP")), `"nonce":"n1"`) {
			w.Header().Set("DPoP-Nonce", "n1")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"use_dpop_nonce"}`))
			return
		}
		s.mu.Lock()
		s.par = r.PostForm
		s.challenge = r.PostForm.Get("code_challenge")
		s.mu.Unlock()
		w.Header().Set("DPoP-Nonce", "n1")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"request_uri":"urn:ietf:params:oauth:request_uri:abc","expires_in":300}`))
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		s.mu.Lock()
		ok := base64.RawURLEncoding.EncodeToString(sum[:]) == s.challenge && r.PostForm.Get("code") == "the-code" && r.PostForm.Get("client_assertion") != ""
		sub := s.subject
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"sub":"` + sub + `","scope":"atproto transition:generic","access_token":"ACCESS-SECRET","refresh_token":"REFRESH-SECRET","token_type":"DPoP"}`))
	})
	mux.HandleFunc("GET /xrpc/app.bsky.notification.listNotifications", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.xrpcAuth = append(s.xrpcAuth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if !strings.Contains(dpopPayload(r.Header.Get("DPoP")), `"nonce":"h2"`) {
			w.Header().Set("WWW-Authenticate", `DPoP error="use_dpop_nonce"`)
			w.Header().Set("DPoP-Nonce", "h2")
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Authorization") != "DPoP ACCESS-SECRET" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"notifications":[]}`))
	})
	s.Server = httptest.NewTLSServer(mux)
	t.Cleanup(s.Close)
	return s
}

func dpopPayload(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	return string(b)
}

func newLinker(t *testing.T, srv *stubServer) (*Linker, string) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := New(Config{ClientID: "https://example.github.io/gw/client-metadata.json", CallbackURL: "https://gw.example.ts.net:8795/oauth/callback"}, dir, st)
	if err != nil {
		t.Fatal(err)
	}
	l.App.Client = srv.Client()
	dirs := identity.NewMockDirectory()
	dirs.Insert(identity.Identity{
		DID: syntax.DID(botDID), Handle: syntax.Handle("bot.example.com"),
		Services: map[string]identity.ServiceEndpoint{"atproto_pds": {Type: "AtprotoPersonalDataServer", URL: srv.URL}},
	})
	l.App.Dir = dirs
	l.AuthServer = func(context.Context, string) (*oauth.AuthServerMetadata, error) {
		return &oauth.AuthServerMetadata{
			Issuer: srv.URL, AuthorizationEndpoint: srv.URL + "/authorize",
			PushedAuthorizationRequestEndpoint: srv.URL + "/par", TokenEndpoint: srv.URL + "/token",
		}, nil
	}
	return l, dir
}

func start(t *testing.T, l *Linker, srv *stubServer) string {
	t.Helper()
	to, err := l.Start(context.Background(), "bot-1", "@bot.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(to, srv.URL+"/authorize?") || !strings.Contains(to, "request_uri=urn") {
		t.Fatalf("redirect %s", to)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.par.Get("client_assertion_type") == "" || srv.par.Get("code_challenge_method") != "S256" || srv.par.Get("login_hint") != "bot.example.com" ||
		srv.par.Get("scope") != "atproto transition:generic transition:chat.bsky" || srv.par.Get("redirect_uri") != "https://gw.example.ts.net:8795/oauth/callback" {
		t.Fatalf("PAR %v", srv.par)
	}
	return srv.par.Get("state")
}

func TestOAuthLoginLinksTheBotAndRequestsUseDPoP(t *testing.T) {
	srv := newStub(t)
	l, dir := newLinker(t, srv)
	state := start(t, l, srv)
	botID, did, err := l.Callback(context.Background(), url.Values{"state": {state}, "iss": {srv.URL}, "code": {"the-code"}})
	if err != nil || botID != "bot-1" || did != botDID {
		t.Fatalf("callback %s %s %v", botID, did, err)
	}
	if link, ok := l.Store.Linked("bot-1"); !ok || link.DID != botDID {
		t.Fatal("not linked")
	}
	for _, f := range []string{"oauth.json", keyFile} {
		fi, err := os.Stat(dir + "/" + f)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v %v", f, fi, err)
		}
	}
	// a restart reads the session back from the file and uses it (DPoP, host nonce retry)
	st2, _ := OpenStore(dir)
	l.Store = st2
	l.App.Store = st2
	sess, err := l.Session(context.Background(), "bot-1")
	if err != nil {
		t.Fatal(err)
	}
	c := &bsky.Client{HTTP: srv.Client(), OAuth: sess}
	if _, err := c.ListNotifications(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if self, _ := c.SelfDID(context.Background()); self != botDID {
		t.Fatalf("self %s", self)
	}
	srv.mu.Lock()
	auth := srv.xrpcAuth
	srv.mu.Unlock()
	if len(auth) != 2 || auth[1] != "DPoP ACCESS-SECRET" {
		t.Fatalf("xrpc auth %v", auth)
	}
	// logout forgets the tokens
	if err := l.Logout(context.Background(), "bot-1"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dir + "/oauth.json")
	if strings.Contains(string(data), "ACCESS-SECRET") {
		t.Fatal("logout kept the tokens")
	}
}

func TestOAuthCallbackRejectsWrongStateIssuerPKCEAndAccount(t *testing.T) {
	srv := newStub(t)
	l, _ := newLinker(t, srv)
	state := start(t, l, srv)
	ctx := context.Background()
	if _, _, err := l.Callback(ctx, url.Values{"state": {"forged"}, "iss": {srv.URL}, "code": {"the-code"}}); err == nil {
		t.Fatal("unknown state accepted")
	}
	if _, _, err := l.Callback(ctx, url.Values{"state": {state}, "iss": {"https://evil.example.com"}, "code": {"the-code"}}); err == nil {
		t.Fatal("other issuer accepted")
	}
	// a failed callback drops the request: the same state cannot be replayed
	if _, _, err := l.Callback(ctx, url.Values{"state": {state}, "iss": {srv.URL}, "code": {"the-code"}}); err == nil {
		t.Fatal("state replayed")
	}

	state = start(t, l, srv)
	srv.mu.Lock()
	srv.challenge = "not-the-challenge" // the verifier we send no longer matches: PKCE fails
	srv.mu.Unlock()
	if _, _, err := l.Callback(ctx, url.Values{"state": {state}, "iss": {srv.URL}, "code": {"the-code"}}); err == nil {
		t.Fatal("PKCE mismatch accepted")
	}

	state = start(t, l, srv)
	srv.mu.Lock()
	srv.subject = "did:plc:someoneelse00000000000000"
	srv.mu.Unlock()
	if _, _, err := l.Callback(ctx, url.Values{"state": {state}, "iss": {srv.URL}, "code": {"the-code"}}); err == nil {
		t.Fatal("another account accepted")
	}
	if _, ok := l.Store.Linked("bot-1"); ok {
		t.Fatal("a failed login linked the bot")
	}
}

func TestClientMetadataPublishesOnlyThePublicKey(t *testing.T) {
	srv := newStub(t)
	l, dir := newLinker(t, srv)
	m := l.ClientMetadata()
	if err := m.Validate(l.App.Config.ClientID); err != nil {
		t.Fatal(err)
	}
	if !m.IsConfidential() || m.TokenEndpointAuthMethod != "private_key_jwt" || m.Scope != "atproto transition:generic transition:chat.bsky" {
		t.Fatalf("metadata %+v", m)
	}
	data, _ := json.Marshal(m)
	key, _ := os.ReadFile(dir + "/" + keyFile)
	if strings.Contains(string(data), strings.TrimSpace(string(key))) || strings.Contains(string(data), `"d":`) {
		t.Fatal("metadata contains the private key")
	}
	// the same key (and so the same published JWK) after a restart
	l2, err := New(Config{ClientID: l.App.Config.ClientID, CallbackURL: l.App.Config.CallbackURL}, dir, l.Store)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(l2.ClientMetadata())
	if string(a) != string(data) {
		t.Fatal("key changed across restarts")
	}
}
