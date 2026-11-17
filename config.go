package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxPermittedFile = int64(1024 * 1024 * 1024)

type Config struct {
	Listen                 string        `json:"listen"`
	WorkRoot               string        `json:"work_root"`
	DefaultWorkdir         string        `json:"default_workdir,omitempty"`
	SkillsRoot             string        `json:"skills_root"`
	StateDir               string        `json:"state_dir"`
	TokenFile              string        `json:"token_file"`
	PublicURL              string        `json:"public_url"`
	AllowedOrigins         []string      `json:"allowed_origins,omitempty"`
	SharedMCPBin           string        `json:"shared_mcp_bin"`
	ExecutableDirs         []string      `json:"executable_dirs,omitempty"`
	OAuth                  *OAuthConfig  `json:"oauth,omitempty"`
	Ignore                 *IgnoreConfig `json:"ignore,omitempty"`
	MaxFileBytes           int64         `json:"max_file_bytes"`
	MaxOutputBytes         int           `json:"max_output_bytes"`
	CommandTimeoutSec      int           `json:"command_timeout_seconds"`
	MaxShellSessions       int           `json:"max_shell_sessions,omitempty"`
	MaxSearchEntries       int           `json:"max_search_entries,omitempty"`
	MaxSearchDepth         int           `json:"max_search_depth,omitempty"`
	MaxSearchFileBytes     int           `json:"max_search_file_bytes,omitempty"`
	MaxImageBytes          int           `json:"max_image_bytes,omitempty"`
	MCPDiscoveryTimeoutSec int           `json:"mcp_discovery_timeout_seconds,omitempty"`
	MCPScanWorkers         int           `json:"mcp_scan_workers,omitempty"`
}

type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// defaultWorkdir is the out-of-the-box starting directory for relative paths
// and for tools that do not take an explicit directory. It is always inside
// WorkRoot; WorkRoot stays the security confinement boundary.
func (c Config) defaultWorkdir() string {
	if strings.TrimSpace(c.DefaultWorkdir) != "" {
		return filepath.Clean(c.DefaultWorkdir)
	}
	return filepath.Clean(c.WorkRoot)
}

// allowsOrigin reports whether a browser or desktop client origin may carry
// authenticated MCP requests. The configured public origin is always
// allowed. Additional remote client origins are opt-in via allowed_origins;
// "*" allows any origin and therefore disables the browser-side origin
// restriction entirely.
func (c Config) allowsOrigin(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return true
	}
	// "*" disables the origin restriction entirely, including for origins
	// that do not canonicalize ("null", malformed values).
	for _, allowed := range c.AllowedOrigins {
		if allowed == "*" {
			return true
		}
	}
	canonical, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	if public, ok := canonicalOrigin(strings.TrimSuffix(strings.TrimSpace(c.PublicURL), "/")); ok && public == canonical {
		return true
	}
	for _, allowed := range c.AllowedOrigins {
		if allowed == canonical {
			return true
		}
	}
	return false
}

// canonicalOrigin parses a bare origin (scheme://host[:port]) and returns its
// canonical RFC 6454 form. Origins are case-insensitive: "HTTPS://Claude.AI"
// and "https://claude.ai" are the same origin. Anything that is not a bare
// origin -- opaque URIs, userinfo, a path, query, fragment, surrounding or
// embedded whitespace, and the literal "null" -- is rejected, so a malformed
// value can never be matched by accident.
func canonicalOrigin(raw string) (string, bool) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil {
		return "", false
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

// normalizeAllowedOrigins trims and validates the configured origin
// allowlist. Every entry must be "*" or a bare origin: scheme and host only,
// without path, query, fragment, credentials or trailing slash. Desktop
// clients use custom schemes (app://obsidian.md, vscode-webview://...,
// tauri://localhost) and are accepted as long as scheme and host are set.
func normalizeAllowedOrigins(entries []string) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, errors.New("allowed_origins entries must not be empty")
		}
		if entry == "*" {
			out = append(out, "*")
			continue
		}
		canonical, ok := canonicalOrigin(entry)
		if !ok {
			return nil, fmt.Errorf("allowed_origins entry must be a bare scheme://host origin without path, query, fragment or credentials: %s", entry)
		}
		out = append(out, canonical)
	}
	return out, nil
}

// verifyDirWithinRoot proves that dir is a directory inside workRoot both
// lexically and after symlink resolution. A lexical check alone cannot see
// through a symlink that lives inside workRoot and points outside of it, so
// the resolved path is also opened through os.Root: the kernel VFS refuses to
// leave the root, which rejects the escape even if the link target changes
// between the check and the actual use.
func verifyDirWithinRoot(workRoot, dir string) error {
	p, err := resolveWorkPath(workRoot, dir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(workRoot), p)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New("path escapes configured work_root")
	}
	rootFD, err := os.OpenRoot(workRoot)
	if err != nil {
		return err
	}
	defer rootFD.Close()
	if rel == "." {
		return nil
	}
	info, err := rootFD.Stat(rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", dir)
	}
	return nil
}

func readConfig(path string) (Config, error) {
	c := (Config{}).withTuningDefaults()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.WorkRoot == "" || c.TokenFile == "" || c.StateDir == "" || c.Listen == "" {
		return c, errors.New("listen, work_root, state_dir and token_file are required")
	}
	if !strings.HasPrefix(c.Listen, "127.0.0.1:") && !strings.HasPrefix(c.Listen, "[::1]:") {
		return c, errors.New("refusing non-loopback listener; use the existing HTTPS reverse proxy")
	}
	if err := c.validateTuning(); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.WorkRoot) || !filepath.IsAbs(c.StateDir) {
		return c, errors.New("work_root and state_dir must be absolute paths")
	}
	if strings.TrimSpace(c.DefaultWorkdir) != "" {
		if !filepath.IsAbs(c.DefaultWorkdir) {
			return c, errors.New("default_workdir must be an absolute path")
		}
		if _, err := resolveWorkPath(c.WorkRoot, c.DefaultWorkdir); err != nil {
			return c, fmt.Errorf("default_workdir must stay inside work_root: %w", err)
		}
		st, err := os.Stat(c.DefaultWorkdir)
		if err != nil || !st.IsDir() {
			return c, fmt.Errorf("default_workdir must be an existing directory: %s", c.DefaultWorkdir)
		}
		if err := verifyDirWithinRoot(c.WorkRoot, c.DefaultWorkdir); err != nil {
			return c, fmt.Errorf("default_workdir must resolve inside work_root: %w", err)
		}
	}
	for _, directory := range c.ExecutableDirs {
		if !filepath.IsAbs(directory) || strings.ContainsRune(directory, os.PathListSeparator) {
			return c, errors.New("executable_dirs must contain absolute directories without path-list separators")
		}
	}
	origins, err := normalizeAllowedOrigins(c.AllowedOrigins)
	if err != nil {
		return c, err
	}
	c.AllowedOrigins = origins
	if c.Ignore != nil {
		if len(c.Ignore.CustomPatterns) > 4096 {
			return c, errors.New("too many custom ignore patterns")
		}
		bytes := 0
		for _, pattern := range c.Ignore.CustomPatterns {
			bytes += len(pattern)
		}
		if bytes > 1<<20 {
			return c, errors.New("custom ignore patterns exceed 1 MiB")
		}
	}
	if err := normalizeOAuthConfig(&c); err != nil {
		return c, err
	}
	return c, nil
}

// normalizeOAuthConfig validates and completes the optional OAuth section.
// Everything omitted in the file gets a safe default, so enabling OAuth only
// requires public_url and (for pre-registered clients) a client list.
func normalizeOAuthConfig(c *Config) error {
	o := c.OAuth
	if o == nil || !o.Enabled {
		return nil
	}
	base := strings.TrimSuffix(strings.TrimSpace(c.PublicURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return errors.New("oauth requires public_url to be a bare origin URL without path, query, or fragment")
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return errors.New("oauth public_url must use http:// or https://")
	}
	c.PublicURL = base

	if len(o.AllowedRedirectURIs) == 0 {
		o.AllowedRedirectURIs = DefaultOAuthRedirectPatterns()
	}
	for _, pattern := range o.AllowedRedirectURIs {
		if err := validateRedirectPattern(pattern); err != nil {
			return fmt.Errorf("oauth allowed_redirect_uris: %w", err)
		}
	}

	if o.AccessTokenTTLSec == 0 {
		o.AccessTokenTTLSec = 30 * 86400
	}
	if o.AccessTokenTTLSec < 60 || o.AccessTokenTTLSec > 365*86400 {
		return errors.New("oauth access_token_ttl_seconds must be between 60 and 31536000")
	}
	if o.RefreshTokenTTLDays == 0 {
		o.RefreshTokenTTLDays = 90
	}
	if o.RefreshTokenTTLDays < 1 || o.RefreshTokenTTLDays > 365 {
		return errors.New("oauth refresh_token_ttl_days must be between 1 and 365")
	}

	seen := make(map[string]bool, len(o.Clients))
	for i := range o.Clients {
		client := &o.Clients[i]
		client.ClientID = strings.TrimSpace(client.ClientID)
		if client.ClientID == "" {
			return errors.New("oauth clients must have a non-empty client_id")
		}
		if seen[client.ClientID] {
			return fmt.Errorf("oauth client_id is duplicated: %s", client.ClientID)
		}
		seen[client.ClientID] = true
		if client.ClientSecretSHA256 != "" {
			if len(client.ClientSecretSHA256) != 64 {
				return fmt.Errorf("oauth client %s client_secret_sha256 must be a SHA-256 hex digest", client.ClientID)
			}
			for _, r := range client.ClientSecretSHA256 {
				if !isHexDigit(r) {
					return fmt.Errorf("oauth client %s client_secret_sha256 must be a SHA-256 hex digest", client.ClientID)
				}
			}
			client.ClientSecretSHA256 = strings.ToLower(client.ClientSecretSHA256)
		}
		for _, uri := range client.RedirectURIs {
			if err := validateRedirectPattern(uri); err != nil {
				return fmt.Errorf("oauth client %s redirect_uris: %w", client.ClientID, err)
			}
		}
	}
	return nil
}

func isHexDigit(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// Defaults and resource ranges are shared by configuration and hot updates.
func (c Config) withTuningDefaults() Config {
	if c.MaxFileBytes == 0 {
		c.MaxFileBytes = 300 * 1024 * 1024
	}
	if c.MaxOutputBytes == 0 {
		c.MaxOutputBytes = 512 * 1024
	}
	if c.CommandTimeoutSec == 0 {
		c.CommandTimeoutSec = 60
	}
	if c.MaxShellSessions == 0 {
		c.MaxShellSessions = 8
	}
	if c.MaxSearchEntries == 0 {
		c.MaxSearchEntries = 50000
	}
	if c.MaxSearchDepth == 0 {
		c.MaxSearchDepth = 64
	}
	if c.MaxSearchFileBytes == 0 {
		c.MaxSearchFileBytes = 4 * 1024 * 1024
	}
	if c.MaxImageBytes == 0 {
		c.MaxImageBytes = 5 << 20
	}
	if c.MCPDiscoveryTimeoutSec == 0 {
		c.MCPDiscoveryTimeoutSec = 45
	}
	if c.MCPScanWorkers == 0 {
		c.MCPScanWorkers = 8
	}
	return c
}

type tuningRange struct {
	name             string
	value, low, high int64
}

func (c Config) tuningRanges() []tuningRange {
	return []tuningRange{
		{"max_file_bytes", c.MaxFileBytes, 1 << 20, maxPermittedFile},
		{"max_output_bytes", int64(c.MaxOutputBytes), 1024, 4 << 20},
		{"command_timeout_seconds", int64(c.CommandTimeoutSec), 1, 3600},
		{"max_shell_sessions", int64(c.MaxShellSessions), 1, 32},
		{"max_search_entries", int64(c.MaxSearchEntries), 1, 1000000},
		{"max_search_depth", int64(c.MaxSearchDepth), 1, 256},
		{"max_search_file_bytes", int64(c.MaxSearchFileBytes), 1024, 16 << 20},
		{"max_image_bytes", int64(c.MaxImageBytes), 1024, 32 << 20},
		{"mcp_discovery_timeout_seconds", int64(c.MCPDiscoveryTimeoutSec), 1, 120},
		{"mcp_scan_workers", int64(c.MCPScanWorkers), 1, 32},
	}
}
func (c Config) validateTuning() error {
	for _, t := range c.tuningRanges() {
		if t.value < t.low || t.value > t.high {
			return fmt.Errorf("%s must be between %d and %d", t.name, t.low, t.high)
		}
	}
	return nil
}

func newConfigStore(path string) (*ConfigStore, error) {
	c, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	return &ConfigStore{path: path, cfg: c}, nil
}
func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.withTuningDefaults()
}

func (s *ConfigStore) Update(in SettingsInput) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg.withTuningDefaults()
	if in.MaxFileBytes != nil {
		next.MaxFileBytes = *in.MaxFileBytes
	}
	if in.MaxOutputBytes != nil {
		next.MaxOutputBytes = *in.MaxOutputBytes
	}
	if in.CommandTimeoutSeconds != nil {
		next.CommandTimeoutSec = *in.CommandTimeoutSeconds
	}
	if in.MaxShellSessions != nil {
		next.MaxShellSessions = *in.MaxShellSessions
	}
	if in.MaxSearchEntries != nil {
		next.MaxSearchEntries = *in.MaxSearchEntries
	}
	if in.MaxSearchDepth != nil {
		next.MaxSearchDepth = *in.MaxSearchDepth
	}
	if in.MaxSearchFileBytes != nil {
		next.MaxSearchFileBytes = *in.MaxSearchFileBytes
	}
	if in.MaxImageBytes != nil {
		next.MaxImageBytes = *in.MaxImageBytes
	}
	if in.MCPDiscoveryTimeoutSeconds != nil {
		next.MCPDiscoveryTimeoutSec = *in.MCPDiscoveryTimeoutSeconds
	}
	if in.MCPScanWorkers != nil {
		next.MCPScanWorkers = *in.MCPScanWorkers
	}
	if err := next.validateTuning(); err != nil {
		return Config{}, err
	}

	if in == (SettingsInput{}) {
		return next, nil
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return Config{}, err
	}
	if err = atomicFile(s.path, append(b, '\n'), 0600); err != nil {
		return Config{}, err
	}
	s.cfg = next
	return next, nil
}

func atomicFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".agent-mcp-go-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(perm); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func readBearerToken(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0077 != 0 {
		return nil, errors.New("bearer token file must be owner-only (0600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) < 32 {
		return nil, errors.New("bearer token must have at least 32 characters")
	}
	return b, nil
}

func tokenEqual(got string, want []byte) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), want) == 1
}
