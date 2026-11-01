package daemon

import (
	_ "embed"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kojira/opencrab-bluesky-gateway/internal/store"
)

//go:embed page.html
var page []byte

// botView is a bot as the API shows it: never the app password.
type botView struct {
	InstanceID  string   `json:"instance_id"`
	AgentID     string   `json:"agent_id"`
	Label       string   `json:"label"`
	Handle      string   `json:"handle"`
	PasswordSet bool     `json:"app_password_set"`
	Service     string   `json:"service"`
	Enabled     bool     `json:"enabled"`
	PollSecs    int      `json:"poll_secs"`
	Owners      []string `json:"owners"`
	Configured  bool     `json:"configured"`
	Status      Status   `json:"status"`
	// OAuth is the linked account (DID only; tokens never leave the store).
	OAuth *oauthView `json:"oauth"`
	// OAuthAvailable: the gateway has an oauth config, so 「Bluesky でログイン」 can be offered.
	OAuthAvailable bool `json:"oauth_available"`
}

type oauthView struct {
	DID string `json:"did"`
}

func (d *Daemon) view(b store.Bot) botView {
	owners := b.Owners
	if owners == nil {
		owners = []string{}
	}
	v := botView{
		InstanceID: b.InstanceID, AgentID: b.AgentID, Label: b.Label, Handle: b.Handle,
		PasswordSet: b.AppPassword != "", Service: b.Service, Enabled: b.Enabled,
		PollSecs: b.PollSecs, Owners: owners, Configured: d.configured(b), Status: d.Status(b.InstanceID),
		OAuthAvailable: d.OAuth != nil,
	}
	if l, ok := d.oauthLinked(b.InstanceID); ok {
		v.OAuth = &oauthView{DID: l.DID.String()}
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func storeFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrExists):
		fail(w, http.StatusConflict, err.Error())
	default:
		fail(w, http.StatusBadRequest, err.Error())
	}
}

// Handler is the settings page and its API.
//
//	GET    /                       the page
//	GET    /api/bots               bots (no passwords)
//	POST   /api/bots               {agent_id, label} → a new bot
//	PATCH  /api/bots/{id}          store.Edit; an empty app_password keeps the old one
//	DELETE /api/bots/{id}
//	GET    /api/agents             core's agents (when core_api is set)
//	GET    /oauth/login/{id}?handle=  starts 「Bluesky でログイン」 (redirects to the auth server)
//	GET    /oauth/callback         the auth server's redirect back
//	POST   /api/bots/{id}/oauth/logout
//	GET    /oauth/client-metadata.json  the document to publish at oauth.client_id
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	d.oauthRoutes(mux)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /api/bots", func(w http.ResponseWriter, _ *http.Request) {
		out := []botView{}
		for _, b := range d.Store.List() {
			out = append(out, d.view(b))
		}
		writeJSON(w, http.StatusOK, map[string]any{"bots": out})
	})
	mux.HandleFunc("POST /api/bots", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AgentID string `json:"agent_id"`
			Label   string `json:"label"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			fail(w, http.StatusBadRequest, "body must be {agent_id, label}")
			return
		}
		b, err := d.Store.Create(body.AgentID, body.Label)
		if err != nil {
			storeFail(w, err)
			return
		}
		d.Apply(b.InstanceID)
		writeJSON(w, http.StatusCreated, d.view(b))
	})
	mux.HandleFunc("PATCH /api/bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		var e store.Edit
		if json.NewDecoder(r.Body).Decode(&e) != nil {
			fail(w, http.StatusBadRequest, "bad body")
			return
		}
		before, _ := d.Store.Get(r.PathValue("id"))
		b, err := d.Store.Update(r.PathValue("id"), e)
		if err != nil {
			storeFail(w, err)
			return
		}
		if b.Handle != before.Handle && d.OAuth != nil {
			// the OAuth session is the old account's; log in again for the new handle
			_ = d.OAuth.Logout(r.Context(), b.InstanceID)
		}
		d.Apply(b.InstanceID)
		writeJSON(w, http.StatusOK, d.view(b))
	})
	mux.HandleFunc("DELETE /api/bots/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := d.Store.Delete(id); err != nil {
			storeFail(w, err)
			return
		}
		if d.OAuth != nil {
			_ = d.OAuth.Logout(r.Context(), id)
		}
		d.Apply(id)
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		if d.Config.CoreAPI == "" {
			writeJSON(w, http.StatusOK, map[string]any{"agents": []any{}})
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, d.Config.CoreAPI+"/api/agents", nil)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			fail(w, http.StatusBadGateway, "core: "+err.Error())
			return
		}
		defer resp.Body.Close()
		var agents []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&agents) != nil {
			fail(w, http.StatusBadGateway, "core answered "+resp.Status)
			return
		}
		has := map[string]bool{}
		for _, b := range d.Store.List() {
			has[b.AgentID] = true
		}
		out := []map[string]any{}
		for _, a := range agents {
			out = append(out, map[string]any{"id": a.ID, "name": a.Name, "has_bot": has[a.ID]})
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": out})
	})
	return mux
}

// oauthPage is the small page the browser lands on after the auth server (or a failed start).
func oauthPage(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><html lang="ja"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Bluesky でログイン</title><body style="font:16px/1.6 sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem"><p>` +
		html.EscapeString(text) + `</p><p><a href="/">設定に戻る</a></p></body></html>`))
}

func (d *Daemon) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/client-metadata.json", func(w http.ResponseWriter, _ *http.Request) {
		if d.OAuth == nil {
			fail(w, http.StatusNotFound, "oauth is not configured")
			return
		}
		writeJSON(w, http.StatusOK, d.OAuth.ClientMetadata())
	})
	mux.HandleFunc("GET /oauth/login/{id}", func(w http.ResponseWriter, r *http.Request) {
		if d.OAuth == nil {
			oauthPage(w, http.StatusNotFound, "この gateway には OAuth の設定がありません。")
			return
		}
		b, err := d.Store.Get(r.PathValue("id"))
		if err != nil {
			oauthPage(w, http.StatusNotFound, "bot が見つかりません。")
			return
		}
		handle := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("handle")), "@")
		if handle == "" {
			handle = b.Handle
		}
		if handle == "" {
			oauthPage(w, http.StatusBadRequest, "ハンドルを入れてください。")
			return
		}
		if handle != b.Handle {
			h := handle
			if _, err := d.Store.Update(b.InstanceID, store.Edit{Handle: &h}); err != nil {
				oauthPage(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		to, err := d.OAuth.Start(r.Context(), b.InstanceID, handle)
		if err != nil {
			d.Log.Warn("oauth start", "bot", b.Label, "err", err)
			oauthPage(w, http.StatusBadGateway, "ログインを始められません: "+err.Error())
			return
		}
		http.Redirect(w, r, to, http.StatusFound)
	})
	mux.HandleFunc("GET /oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		if d.OAuth == nil {
			oauthPage(w, http.StatusNotFound, "この gateway には OAuth の設定がありません。")
			return
		}
		botID, did, err := d.OAuth.Callback(r.Context(), r.URL.Query())
		if err != nil {
			d.Log.Warn("oauth callback", "err", err)
			oauthPage(w, http.StatusBadRequest, "ログインできませんでした: "+err.Error())
			return
		}
		d.Apply(botID)
		b, _ := d.Store.Get(botID)
		d.Log.Info("oauth linked", "bot", b.Label, "did", did)
		http.Redirect(w, r, "/?oauth="+url.QueryEscape(b.Label), http.StatusFound)
	})
	mux.HandleFunc("POST /api/bots/{id}/oauth/logout", func(w http.ResponseWriter, r *http.Request) {
		if d.OAuth == nil {
			fail(w, http.StatusNotFound, "oauth is not configured")
			return
		}
		b, err := d.Store.Get(r.PathValue("id"))
		if err != nil {
			storeFail(w, err)
			return
		}
		if err := d.OAuth.Logout(r.Context(), b.InstanceID); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.Apply(b.InstanceID)
		writeJSON(w, http.StatusOK, d.view(b))
	})
}
