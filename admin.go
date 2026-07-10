package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AdminServer serves the JSON control-plane API and the embedded web console.
// Every /api route except /api/serverinfo requires the single admin token.
type AdminServer struct {
	app         *App
	proxyListen string
	version     string
}

func newAdminServer(app *App, proxyListen, version string) *AdminServer {
	return &AdminServer{app: app, proxyListen: proxyListen, version: version}
}

func (a *AdminServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleUI)
	mux.HandleFunc("/api/serverinfo", a.handleServerInfo) // public
	mux.HandleFunc("/api/templates", a.auth(a.handleTemplates))
	mux.HandleFunc("/api/info", a.auth(a.handleInfo))
	mux.HandleFunc("/api/stats", a.auth(a.handleStats))
	mux.HandleFunc("/api/stats/reset", a.auth(a.handleStatsReset))
	mux.HandleFunc("/api/recent", a.auth(a.handleRecent))
	mux.HandleFunc("/api/rules", a.auth(a.handleRules))
	mux.HandleFunc("/api/rules/", a.auth(a.handleRuleItem))
	return mux
}

func tokenFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if h := r.Header.Get("X-Admin-Token"); h != "" {
		return h
	}
	return r.URL.Query().Get("token")
}

func (a *AdminServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.app.checkToken(tokenFrom(r)) {
			writeJSON(w, 401, map[string]string{"error": "invalid or missing token"})
			return
		}
		next(w, r)
	}
}

func (a *AdminServer) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"version":    a.version,
		"proxy_port": portOf(a.proxyListen),
	})
}

func (a *AdminServer) handleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, errorTemplates)
}

// handleInfo returns everything needed to point an S3 client at the proxy,
// including the proxy secret (the admin-token holder owns this instance).
func (a *AdminServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"proxy_access_key": a.app.ProxyAccessKeyID,
		"proxy_secret_key": a.app.ProxySecretKey,
		"endpoint_host":    a.app.endpointHost(),
		"region":           regionOr(a.app.Region),
	})
}

func (a *AdminServer) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.app.stats.snapshot())
}

func (a *AdminServer) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	a.app.stats.reset()
	writeJSON(w, 200, map[string]string{"ok": "reset"})
}

func (a *AdminServer) handleRecent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.app.stats.recentCopy())
}

func (a *AdminServer) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.app.engine.list())
	case http.MethodPost:
		var rule Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
		if rule.ID == "" {
			rule.ID = newRuleID()
		}
		if rule.Probability <= 0 {
			rule.Probability = 1.0
		}
		rule.Enabled = true
		rule.Hits = 0
		a.app.engine.add(&rule)
		_ = a.app.Save()
		writeJSON(w, 201, rule)
	case http.MethodDelete:
		a.app.engine.clear()
		_ = a.app.Save()
		writeJSON(w, 200, map[string]string{"ok": "cleared"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *AdminServer) handleRuleItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	id := rest
	action := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	switch {
	case r.Method == http.MethodDelete:
		if a.app.engine.remove(id) {
			_ = a.app.Save()
			writeJSON(w, 200, map[string]string{"deleted": id})
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	case r.Method == http.MethodPost && action == "toggle":
		if enabled, ok := a.app.engine.toggle(id); ok {
			_ = a.app.Save()
			writeJSON(w, 200, map[string]any{"id": id, "enabled": enabled})
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *AdminServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func portOf(listen string) string {
	if i := strings.LastIndexByte(listen, ':'); i >= 0 {
		return listen[i+1:]
	}
	return listen
}
