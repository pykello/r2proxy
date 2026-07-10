package main

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// App is the entire server state: a single upstream target, one proxy credential
// pair that S3 clients authenticate with, one admin token for the console, and
// the injection rules. No tenants.
type App struct {
	mu   sync.Mutex
	path string

	// Upstream (real R2) target.
	Endpoint       string `json:"endpoint"`
	UpstreamKeyID  string `json:"upstream_access_key_id"`
	UpstreamSecret string `json:"upstream_secret_key"`
	Region         string `json:"region"`

	// Client-facing proxy credentials. S3 clients sign with these; the proxy
	// verifies the signature against ProxySecretKey, then re-signs to R2.
	ProxyAccessKeyID string `json:"proxy_access_key_id"`
	ProxySecretKey   string `json:"proxy_secret_key"`

	// The single token for the console / admin API.
	AdminToken string `json:"admin_token"`

	Rules []*Rule `json:"rules"`

	// runtime, not persisted
	stats  *Stats  `json:"-"`
	engine *Engine `json:"-"`
}

func newApp(path string) *App {
	return &App{path: path, Region: "auto"}
}

func (a *App) init() {
	if a.stats == nil {
		a.stats = newStats()
	}
	if a.engine == nil {
		a.engine = newEngine(a.Rules)
	}
	if a.Region == "" {
		a.Region = "auto"
	}
}

// Load reads the config file if present. A missing file is fine (fresh start).
func (a *App) Load() error {
	data, err := os.ReadFile(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.init()
			return nil
		}
		return err
	}
	if err := json.Unmarshal(data, a); err != nil {
		return err
	}
	a.init()
	return nil
}

// Save persists the config atomically (mode 0600).
func (a *App) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.save()
}

func (a *App) save() error {
	if a.path == "" {
		return nil
	}
	if a.engine != nil {
		a.Rules = a.engine.rulesSnapshot()
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(a.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// Configure sets/refreshes the upstream target and generates proxy credentials
// and an admin token on first use. Returns whether new credentials were minted.
func (a *App) Configure(endpoint, keyID, secret, region string) (minted bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if endpoint != "" {
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			endpoint = "https://" + endpoint
		}
		a.Endpoint = endpoint
	}
	if keyID != "" {
		a.UpstreamKeyID = keyID
	}
	if secret != "" {
		a.UpstreamSecret = secret
	}
	if region != "" {
		a.Region = region
	}
	if a.Endpoint == "" || a.UpstreamKeyID == "" || a.UpstreamSecret == "" {
		return false, errors.New("endpoint, upstream access key and secret are required (flags/env or existing config)")
	}
	if a.ProxyAccessKeyID == "" {
		a.ProxyAccessKeyID = newProxyAccessKey()
		minted = true
	}
	if a.ProxySecretKey == "" {
		a.ProxySecretKey = newProxySecret()
		minted = true
	}
	if a.AdminToken == "" {
		a.AdminToken = newToken()
		minted = true
	}
	a.init()
	return minted, a.save()
}

// endpointHost returns the host part of the upstream endpoint (no scheme/port).
func (a *App) endpointHost() string {
	u, err := url.Parse(a.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// checkToken reports whether tok is the admin token.
func (a *App) checkToken(tok string) bool {
	return tok != "" && tok == a.AdminToken
}
