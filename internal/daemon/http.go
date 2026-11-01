package daemon

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
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
}

func (d *Daemon) view(b store.Bot) botView {
	owners := b.Owners
	if owners == nil {
		owners = []string{}
	}
	return botView{
		InstanceID: b.InstanceID, AgentID: b.AgentID, Label: b.Label, Handle: b.Handle,
		PasswordSet: b.AppPassword != "", Service: b.Service, Enabled: b.Enabled,
		PollSecs: b.PollSecs, Owners: owners, Configured: b.Configured(), Status: d.Status(b.InstanceID),
	}
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
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
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
		b, err := d.Store.Update(r.PathValue("id"), e)
		if err != nil {
			storeFail(w, err)
			return
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
