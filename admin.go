package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// AdminServer serves the JSON control-plane API and the embedded web UI.
// Every /api route (except a couple of public ones) requires a bearer token:
// the global admin token (superuser) or a tenant's scoped token.
type AdminServer struct {
	mgr         *Manager
	proxyListen string
	version     string
}

func newAdminServer(mgr *Manager, proxyListen, version string) *AdminServer {
	return &AdminServer{mgr: mgr, proxyListen: proxyListen, version: version}
}

func (a *AdminServer) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleUI)
	mux.HandleFunc("/api/serverinfo", a.handleServerInfo) // public
	mux.HandleFunc("/api/me", a.auth(a.handleMe))
	mux.HandleFunc("/api/templates", a.auth(a.handleTemplates))
	mux.HandleFunc("/api/tenants", a.auth(a.handleTenants))
	mux.HandleFunc("/api/tenants/", a.auth(a.handleTenantItem))
	mux.HandleFunc("/api/stats", a.auth(a.handleStats))
	mux.HandleFunc("/api/stats/reset", a.auth(a.handleStatsReset))
	mux.HandleFunc("/api/recent", a.auth(a.handleRecent))
	mux.HandleFunc("/api/info", a.auth(a.handleTenantInfo))
	mux.HandleFunc("/api/rules", a.auth(a.handleRules))
	mux.HandleFunc("/api/rules/", a.auth(a.handleRuleItem))
	return mux
}

// ---- auth middleware ----

type ctxScope struct {
	scope Scope
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

func (a *AdminServer) auth(next func(http.ResponseWriter, *http.Request, Scope)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, ok := a.mgr.Scope(tokenFrom(r))
		if !ok {
			writeJSON(w, 401, map[string]string{"error": "invalid or missing token"})
			return
		}
		next(w, r, scope)
	}
}

// resolveTenant picks the tenant a scoped request targets: for a tenant token it
// is always their own; for superuser it comes from ?tenant=<id>.
func (a *AdminServer) resolveTenant(r *http.Request, scope Scope) *Tenant {
	if scope.Tenant != nil {
		return scope.Tenant
	}
	if id := r.URL.Query().Get("tenant"); id != "" {
		return a.mgr.GetByID(id)
	}
	return nil
}

// ---- handlers ----

func (a *AdminServer) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"version":      a.version,
		"proxy_listen": a.proxyListen,
		"proxy_port":   portOf(a.proxyListen),
	})
}

func (a *AdminServer) handleMe(w http.ResponseWriter, r *http.Request, scope Scope) {
	resp := map[string]any{"super": scope.Super}
	if scope.Tenant != nil {
		resp["tenant"] = tenantPublic(scope.Tenant)
	}
	writeJSON(w, 200, resp)
}

func (a *AdminServer) handleTemplates(w http.ResponseWriter, r *http.Request, _ Scope) {
	writeJSON(w, 200, errorTemplates)
}

func (a *AdminServer) handleTenants(w http.ResponseWriter, r *http.Request, scope Scope) {
	if !scope.Super {
		writeJSON(w, 403, map[string]string{"error": "superuser only"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var out []map[string]any
		for _, t := range a.mgr.List() {
			out = append(out, tenantAdminView(t))
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var spec TenantSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json: " + err.Error()})
			return
		}
		t, err := a.mgr.CreateTenant(spec)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		// Return full secrets exactly once, at creation.
		writeJSON(w, 201, tenantCreated(t))
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *AdminServer) handleTenantItem(w http.ResponseWriter, r *http.Request, scope Scope) {
	if !scope.Super {
		writeJSON(w, 403, map[string]string{"error": "superuser only"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/tenants/")
	switch r.Method {
	case http.MethodDelete:
		if a.mgr.DeleteTenant(id) {
			writeJSON(w, 200, map[string]string{"deleted": id})
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *AdminServer) handleStats(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	writeJSON(w, 200, t.stats.snapshot())
}

func (a *AdminServer) handleStatsReset(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	t.stats.reset()
	writeJSON(w, 200, map[string]string{"ok": "reset"})
}

func (a *AdminServer) handleRecent(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	writeJSON(w, 200, t.stats.recentCopy())
}

func (a *AdminServer) handleTenantInfo(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	writeJSON(w, 200, tenantPublic(t))
}

func (a *AdminServer) handleRules(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, t.engine.list())
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
		if rule.Remaining == 0 {
			rule.Remaining = -1
		}
		rule.Enabled = true
		rule.Hits = 0
		t.engine.add(&rule)
		_ = a.mgr.Save()
		writeJSON(w, 201, rule)
	case http.MethodDelete:
		t.engine.clear()
		_ = a.mgr.Save()
		writeJSON(w, 200, map[string]string{"ok": "cleared"})
	default:
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func (a *AdminServer) handleRuleItem(w http.ResponseWriter, r *http.Request, scope Scope) {
	t := a.resolveTenant(r, scope)
	if t == nil {
		writeJSON(w, 400, map[string]string{"error": "tenant required"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	id := rest
	action := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	switch {
	case r.Method == http.MethodDelete:
		if t.engine.remove(id) {
			_ = a.mgr.Save()
			writeJSON(w, 200, map[string]string{"deleted": id})
		} else {
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	case r.Method == http.MethodPost && action == "toggle":
		if enabled, ok := t.engine.toggle(id); ok {
			_ = a.mgr.Save()
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

// ---- serialization helpers ----

// tenantPublic exposes only non-secret connection info (safe for tenant view).
func tenantPublic(t *Tenant) map[string]any {
	return map[string]any{
		"id":               t.ID,
		"name":             t.Name,
		"endpoint_host":    t.endpointHost(),
		"region":           regionOr(t.Region),
		"proxy_access_key": t.ProxyAccessKeyID,
		"bucket_allowlist": t.BucketAllowlist,
		"created":          t.Created.Format(time.RFC3339),
	}
}

// tenantAdminView is the superuser list view: identifiers + tenant token, but
// never the proxy/upstream secrets.
func tenantAdminView(t *Tenant) map[string]any {
	return map[string]any{
		"id":               t.ID,
		"name":             t.Name,
		"endpoint":         t.Endpoint,
		"region":           regionOr(t.Region),
		"proxy_access_key": t.ProxyAccessKeyID,
		"token":            t.Token,
		"bucket_allowlist": t.BucketAllowlist,
		"created":          t.Created.Format(time.RFC3339),
		"total_requests":   t.stats.snapshot().Total,
	}
}

// tenantCreated is returned once on creation and includes the secrets the tenant
// needs to configure their client and access their dashboard.
func tenantCreated(t *Tenant) map[string]any {
	return map[string]any{
		"id":               t.ID,
		"name":             t.Name,
		"endpoint":         t.Endpoint,
		"region":           regionOr(t.Region),
		"proxy_access_key": t.ProxyAccessKeyID,
		"proxy_secret_key": t.ProxySecretKey,
		"token":            t.Token,
		"created":          t.Created.Format(time.RFC3339),
	}
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
