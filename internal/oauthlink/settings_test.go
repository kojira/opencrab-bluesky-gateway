package oauthlink_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kojira/opencrab-bluesky-gateway/internal/daemon"
	"github.com/kojira/opencrab-bluesky-gateway/internal/oauthlink"
	"github.com/kojira/opencrab-bluesky-gateway/internal/store"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader("")))
	return rec
}

// The settings page's OAuth login: start redirects to the auth server, the callback links the
// bot, the API shows only the DID (never a token), logout unlinks.
func TestSettingsOAuthLoginNeverReturnsTokens(t *testing.T) {
	srv := oauthlink.NewStubForTest(t)
	l := oauthlink.NewLinkerForTest(t, srv)
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.Create("agent-1", "Blueskyちゃん")
	d := &daemon.Daemon{Config: daemon.Config{CoreSocket: "/nonexistent.sock"}, Store: st, OAuth: l, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.Start(context.Background())
	t.Cleanup(d.Stop)
	h := d.Handler()

	if body := get(t, h, "GET", "/api/bots").Body.String(); !strings.Contains(body, `"oauth_available":true`) || !strings.Contains(body, `"oauth":null`) {
		t.Fatalf("before login %s", body)
	}
	rec := get(t, h, "GET", "/oauth/login/"+b.InstanceID+"?handle=@bot.example.com")
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), srv.URL+"/authorize?") {
		t.Fatalf("login %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec = get(t, h, "GET", "/oauth/callback?state="+oauthlink.StateOf(srv)+"&iss="+srv.URL+"&code=the-code")
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/?oauth=") {
		t.Fatalf("callback %d %s", rec.Code, rec.Body.String())
	}
	body := get(t, h, "GET", "/api/bots").Body.String()
	if !strings.Contains(body, `"did":"`+oauthlink.BotDIDForTest+`"`) || !strings.Contains(body, `"configured":true`) || !strings.Contains(body, `"handle":"bot.example.com"`) {
		t.Fatalf("after login %s", body)
	}
	if strings.Contains(body, "SECRET") || strings.Contains(body, "dpop") || strings.Contains(body, "token") {
		t.Fatalf("API leaks session data: %s", body)
	}
	if m := get(t, h, "GET", "/oauth/client-metadata.json").Body.String(); !strings.Contains(m, `"private_key_jwt"`) || strings.Contains(m, `"d":`) {
		t.Fatalf("metadata %s", m)
	}
	if rec := get(t, h, "POST", "/api/bots/"+b.InstanceID+"/oauth/logout"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"oauth":null`) || !strings.Contains(rec.Body.String(), `"configured":false`) {
		t.Fatalf("logout %d %s", rec.Code, rec.Body.String())
	}
	// a forged callback does not link anything
	if rec := get(t, h, "GET", "/oauth/callback?state=forged&iss="+srv.URL+"&code=the-code"); rec.Code != http.StatusBadRequest {
		t.Fatalf("forged callback %d", rec.Code)
	}
}
