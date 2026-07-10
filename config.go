package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tenant is one isolated proxy target: its own client-facing proxy credentials,
// its own upstream (real R2) credentials, its own admin token, injection rules
// and statistics. Tenants cannot see each other's data.
type Tenant struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`

	// Client-facing credentials. A tenant's program uses these to talk to the
	// proxy; the proxy verifies the request's SigV4 signature against the secret.
	ProxyAccessKeyID string `json:"proxy_access_key_id"`
	ProxySecretKey   string `json:"proxy_secret_key"`

	// Upstream (real target) credentials — never exposed via the read API.
	Endpoint        string `json:"endpoint"`
	UpstreamKeyID   string `json:"upstream_access_key_id"`
	UpstreamSecret  string `json:"upstream_secret_key"`
	Region          string `json:"region"`
	BucketAllowlist string `json:"bucket_allowlist,omitempty"` // optional comma list; "" = any

	// Scoped admin token — grants view/manage of THIS tenant only.
	Token string `json:"token"`

	Rules []*Rule `json:"rules"`

	// runtime, not persisted
	stats  *Stats  `json:"-"`
	engine *Engine `json:"-"`
}

func (t *Tenant) init() {
	if t.stats == nil {
		t.stats = newStats()
	}
	if t.engine == nil {
		t.engine = newEngine(t.Rules)
	}
}

// endpointHost returns the host part of the upstream endpoint (no scheme/port).
func (t *Tenant) endpointHost() string {
	u, err := url.Parse(t.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Manager owns all tenants and the global admin token, and persists to disk.
type Manager struct {
	mu         sync.RWMutex
	path       string
	AdminToken string
	tenants    map[string]*Tenant // by ID
	byProxyKey map[string]*Tenant
	byToken    map[string]*Tenant
}

type persistShape struct {
	AdminToken string    `json:"admin_token"`
	Tenants    []*Tenant `json:"tenants"`
}

func newManager(path string) *Manager {
	return &Manager{
		path:       path,
		tenants:    map[string]*Tenant{},
		byProxyKey: map[string]*Tenant{},
		byToken:    map[string]*Tenant{},
	}
}

// Load reads the config file if present, indexing tenants. Missing file is fine.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var p persistShape
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parse config %s: %w", m.path, err)
	}
	m.AdminToken = p.AdminToken
	for _, t := range p.Tenants {
		t.init()
		m.index(t)
	}
	return nil
}

func (m *Manager) index(t *Tenant) {
	m.tenants[t.ID] = t
	m.byProxyKey[t.ProxyAccessKeyID] = t
	m.byToken[t.Token] = t
}

func (m *Manager) unindex(t *Tenant) {
	delete(m.tenants, t.ID)
	delete(m.byProxyKey, t.ProxyAccessKeyID)
	delete(m.byToken, t.Token)
}

// save writes the config atomically. Caller must hold m.mu.
func (m *Manager) save() error {
	if m.path == "" {
		return nil
	}
	tenants := make([]*Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		t.Rules = t.engine.rulesSnapshot()
		tenants = append(tenants, t)
	}
	p := persistShape{AdminToken: m.AdminToken, Tenants: tenants}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(m.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// Save persists current state (locks).
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.save()
}

// EnsureAdminToken generates a global admin token if none is set.
func (m *Manager) EnsureAdminToken() (created bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.AdminToken == "" {
		m.AdminToken = newToken()
		_ = m.save()
		return true
	}
	return false
}

// TenantSpec holds the inputs to create a tenant.
type TenantSpec struct {
	Name            string
	Endpoint        string
	UpstreamKeyID   string
	UpstreamSecret  string
	Region          string
	BucketAllowlist string
	// Optional fixed proxy creds; generated if empty.
	ProxyAccessKeyID string
	ProxySecretKey   string
}

func (m *Manager) CreateTenant(s TenantSpec) (*Tenant, error) {
	if s.Endpoint == "" || s.UpstreamKeyID == "" || s.UpstreamSecret == "" {
		return nil, errors.New("endpoint, upstream access key and secret are required")
	}
	if s.Region == "" {
		s.Region = "auto"
	}
	if !strings.HasPrefix(s.Endpoint, "http://") && !strings.HasPrefix(s.Endpoint, "https://") {
		s.Endpoint = "https://" + s.Endpoint
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &Tenant{
		ID:               newTenantID(),
		Name:             s.Name,
		Created:          time.Now(),
		ProxyAccessKeyID: s.ProxyAccessKeyID,
		ProxySecretKey:   s.ProxySecretKey,
		Endpoint:         s.Endpoint,
		UpstreamKeyID:    s.UpstreamKeyID,
		UpstreamSecret:   s.UpstreamSecret,
		Region:           s.Region,
		BucketAllowlist:  s.BucketAllowlist,
		Token:            newToken(),
	}
	if t.ProxyAccessKeyID == "" {
		t.ProxyAccessKeyID = newProxyAccessKey()
	}
	if t.ProxySecretKey == "" {
		t.ProxySecretKey = newProxySecret()
	}
	if t.Name == "" {
		t.Name = t.ID
	}
	if _, exists := m.byProxyKey[t.ProxyAccessKeyID]; exists {
		return nil, errors.New("proxy access key already in use")
	}
	t.init()
	m.index(t)
	if err := m.save(); err != nil {
		m.unindex(t)
		return nil, err
	}
	return t, nil
}

func (m *Manager) DeleteTenant(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tenants[id]
	if t == nil {
		return false
	}
	m.unindex(t)
	_ = m.save()
	return true
}

func (m *Manager) GetByID(id string) *Tenant {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tenants[id]
}

func (m *Manager) GetByProxyKey(key string) *Tenant {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byProxyKey[key]
}

// List returns all tenants (pointers; callers must treat as read-mostly).
func (m *Manager) List() []*Tenant {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Tenant, 0, len(m.tenants))
	for _, t := range m.tenants {
		out = append(out, t)
	}
	return out
}

// Scope resolves an admin token to either superuser or a single tenant.
type Scope struct {
	Super  bool
	Tenant *Tenant
}

func (m *Manager) Scope(token string) (Scope, bool) {
	if token == "" {
		return Scope{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.AdminToken != "" && token == m.AdminToken {
		return Scope{Super: true}, true
	}
	if t := m.byToken[token]; t != nil {
		return Scope{Tenant: t}, true
	}
	return Scope{}, false
}
