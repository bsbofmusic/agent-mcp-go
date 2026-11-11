package main

// OAuth 2.1 authorization server for Google Gemini Web connected apps, Claude
// Web and generic MCP OAuth clients.
//
// The server is deliberately small: a single owner (the static Bearer token)
// approves every grant, and the only durable material is a set of SHA-256
// digests. Plaintext access tokens, refresh tokens and client secrets are
// returned once to the client and never written to disk.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/time/rate"
)

const (
	oauthMaxDynamicClients = 128
	oauthMaxAccessTokens   = 512
	oauthMaxRefreshTokens  = 256
	oauthMaxCodes          = 256
	oauthMaxTickets        = 256

	oauthCodeTTL       = 5 * time.Minute
	oauthTicketTTL     = 10 * time.Minute
	oauthMaxBodySmall  = 16 << 10
	oauthMaxBodyTokens = 32 << 10

	oauthLoopbackPattern127       = "http://127.0.0.1:*"
	oauthLoopbackPatternLocalhost = "http://localhost:*"
)

// OAuthClientConfig is a client registered in the configuration file. The
// secret is stored as a SHA-256 hex digest so the file never contains the
// plaintext value.
type OAuthClientConfig struct {
	ClientID           string   `json:"client_id"`
	ClientName         string   `json:"client_name,omitempty"`
	ClientSecretSHA256 string   `json:"client_secret_sha256,omitempty"`
	RedirectURIs       []string `json:"redirect_uris,omitempty"`
}

// OAuthConfig enables the bundled OAuth 2.1 authorization server.
type OAuthConfig struct {
	Enabled             bool                `json:"enabled"`
	AllowedRedirectURIs []string            `json:"allowed_redirect_uris,omitempty"`
	AllowDynamicReg     *bool               `json:"allow_dynamic_registration,omitempty"`
	AccessTokenTTLSec   int                 `json:"access_token_ttl_seconds,omitempty"`
	RefreshTokenTTLDays int                 `json:"refresh_token_ttl_days,omitempty"`
	Clients             []OAuthClientConfig `json:"clients,omitempty"`
}

// DefaultOAuthRedirectPatterns lists the hosted callbacks of the clients AMG
// supports out of the box plus loopback callbacks for desktop clients.
func DefaultOAuthRedirectPatterns() []string {
	return []string{
		"https://*.googleusercontent.com/*",
		"https://*.google.com/*",
		"https://oauth-redirect.googleusercontent.com/*",
		"https://oauth-redirect-sandbox.googleusercontent.com/*",
		"https://oauth-redirect-test.googleusercontent.com/*",
		"https://gemini.google.com/*",
		"https://claude.ai/*",
		"https://*.anthropic.com/*",
		"https://chatgpt.com/*",
		"https://*.openai.com/*",
		"http://127.0.0.1:*",
		"http://localhost:*",
	}
}

type oauthClientRecord struct {
	ClientID                string    `json:"client_id"`
	ClientName              string    `json:"client_name,omitempty"`
	ClientSecretSHA256      string    `json:"client_secret_sha256,omitempty"`
	RedirectURIs            []string  `json:"redirect_uris,omitempty"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method,omitempty"`
	Dynamic                 bool      `json:"dynamic,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
}

type oauthAccessToken struct {
	TokenSHA256        string    `json:"token_sha256"`
	ClientID           string    `json:"client_id"`
	Scopes             []string  `json:"scopes,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	CreatedAt          time.Time `json:"created_at"`
	RefreshTokenSHA256 string    `json:"refresh_token_sha256,omitempty"`
}

type oauthRefreshToken struct {
	TokenSHA256 string    `json:"token_sha256"`
	ClientID    string    `json:"client_id"`
	Scopes      []string  `json:"scopes,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
	CreatedAt   time.Time `json:"created_at"`
}

type oauthState struct {
	Clients       []oauthClientRecord `json:"clients,omitempty"`
	AccessTokens  []oauthAccessToken  `json:"access_tokens,omitempty"`
	RefreshTokens []oauthRefreshToken `json:"refresh_tokens,omitempty"`
}

type oauthCode struct {
	Code          string
	ClientID      string
	RedirectURI   string
	Scopes        []string
	CodeChallenge string
	Resource      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type oauthTicket struct {
	ClientID      string
	RedirectURI   string
	State         string
	Scopes        []string
	ScopeStr      string
	CodeChallenge string
	Resource      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

type oauthServer struct {
	app       *App
	statePath string

	mu      sync.Mutex
	state   oauthState
	codes   map[string]*oauthCode
	tickets map[string]*oauthTicket

	registerLimiter *rate.Limiter
	ticketLimiter   *rate.Limiter
	passkeyLimiter  *rate.Limiter
}

func newOAuthServer(a *App) (*oauthServer, error) {
	c := a.config.Get()
	o := &oauthServer{
		app:             a,
		statePath:       filepath.Join(c.StateDir, "oauth", "state.json"),
		codes:           make(map[string]*oauthCode),
		tickets:         make(map[string]*oauthTicket),
		registerLimiter: rate.NewLimiter(rate.Every(2*time.Second), 10),
		ticketLimiter:   rate.NewLimiter(rate.Limit(5), 30),
		passkeyLimiter:  rate.NewLimiter(rate.Every(time.Second), 5),
	}
	b, err := os.ReadFile(o.statePath)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &o.state); err != nil {
			return nil, fmt.Errorf("oauth state %s is unreadable: %w", o.statePath, err)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	o.mu.Lock()
	changed := o.sweepLocked(time.Now())
	var saveErr error
	if changed {
		saveErr = o.saveLocked()
	}
	o.mu.Unlock()
	if saveErr != nil {
		return nil, saveErr
	}
	return o, nil
}

func (o *oauthServer) publicBase() string {
	return strings.TrimSuffix(strings.TrimSpace(o.app.config.Get().PublicURL), "/")
}

func randomHex(n int) string {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error on supported platforms; a
	// failure here means the process cannot mint unguessable credentials.
	if _, err := rand.Read(b); err != nil {
		panic("oauth: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (o *oauthServer) saveLocked() error {
	b, err := json.MarshalIndent(o.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(o.statePath, append(b, '\n'), 0600)
}

// sweepLocked drops expired access and refresh tokens, plus access tokens whose
// refresh token was dropped. It reports whether the state changed.
func (o *oauthServer) sweepLocked(now time.Time) bool {
	changed := false
	access := make([]oauthAccessToken, 0, len(o.state.AccessTokens))
	for _, t := range o.state.AccessTokens {
		if now.Before(t.ExpiresAt) {
			access = append(access, t)
		} else {
			changed = true
		}
	}
	refresh := make([]oauthRefreshToken, 0, len(o.state.RefreshTokens))
	for _, t := range o.state.RefreshTokens {
		if now.Before(t.ExpiresAt) {
			refresh = append(refresh, t)
		} else {
			changed = true
		}
	}
	if len(refresh) != len(o.state.RefreshTokens) {
		live := make(map[string]bool, len(refresh))
		for _, t := range refresh {
			live[t.TokenSHA256] = true
		}
		kept := access[:0]
		for _, t := range access {
			if t.RefreshTokenSHA256 == "" || live[t.RefreshTokenSHA256] {
				kept = append(kept, t)
			} else {
				changed = true
			}
		}
		access = kept
	}
	o.state.AccessTokens = access
	o.state.RefreshTokens = refresh
	return changed
}

// client resolves a static configuration client first, then a dynamically
// registered one.
func (o *oauthServer) client(clientID string) (oauthClientRecord, bool) {
	if clientID == "" {
		return oauthClientRecord{}, false
	}
	if c := o.app.config.Get(); c.OAuth != nil {
		for _, cl := range c.OAuth.Clients {
			if cl.ClientID != clientID {
				continue
			}
			return oauthClientRecord{
				ClientID:                cl.ClientID,
				ClientName:              cl.ClientName,
				ClientSecretSHA256:      cl.ClientSecretSHA256,
				RedirectURIs:            cl.RedirectURIs,
				TokenEndpointAuthMethod: clientAuthMethod(cl.ClientSecretSHA256),
			}, true
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, cl := range o.state.Clients {
		if cl.ClientID == clientID {
			return cl, true
		}
	}
	return oauthClientRecord{}, false
}

func clientAuthMethod(secretSHA256 string) string {
	if secretSHA256 == "" {
		return "none"
	}
	return "client_secret_post"
}

func (o *oauthServer) redirectAllowed(client oauthClientRecord, redirectURI string) bool {
	c := o.app.config.Get()
	if c.OAuth == nil || redirectURI == "" {
		return false
	}
	if _, ok := validRedirectCandidate(redirectURI); !ok {
		return false
	}
	allowlist := c.OAuth.AllowedRedirectURIs
	if len(allowlist) == 0 {
		allowlist = DefaultOAuthRedirectPatterns()
	}
	if !matchesAnyRedirectPattern(redirectURI, allowlist) {
		return false
	}
	if len(client.RedirectURIs) == 0 {
		return true
	}
	if client.Dynamic {
		for _, registered := range client.RedirectURIs {
			if registered == redirectURI {
				return true
			}
		}
		return false
	}
	return matchesAnyRedirectPattern(redirectURI, client.RedirectURIs)
}

func (o *oauthServer) resourceAllowed(resource string) bool {
	if resource == "" {
		return true
	}
	base := o.publicBase()
	return oauthex.MatchesResource([]string{base, base + "/mcp", base + "/mcp/stateless"}, resource)
}

func (o *oauthServer) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	got, ok := canonicalOrigin(origin)
	if !ok {
		return false
	}
	if want, ok := canonicalOrigin(o.publicBase()); ok && got == want {
		return true
	}
	if want, ok := canonicalOrigin("http://" + o.app.config.Get().Listen); ok && got == want {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Redirect URI validation

func validRedirectCandidate(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return nil, false
	}
	if u.Fragment != "" || u.RawFragment != "" || u.User != nil {
		return nil, false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if scheme != "https" {
		if scheme != "http" || (host != "127.0.0.1" && host != "localhost") {
			return nil, false
		}
	}
	if hasParentSegment(u.Path) {
		return nil, false
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return nil, false
		}
	}
	return u, true
}

func hasParentSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// validateRedirectPattern accepts an exact redirect URI, a loopback wildcard,
// or an HTTPS prefix wildcard ending in "/*" (optionally with a "*.domain"
// subdomain wildcard).
func validateRedirectPattern(raw string) error {
	p := strings.TrimSpace(raw)
	if p == "" {
		return errors.New("redirect URI pattern must not be empty")
	}
	if p == oauthLoopbackPattern127 || p == oauthLoopbackPatternLocalhost || p == "https://*" {
		return nil
	}
	if strings.HasSuffix(p, "/*") {
		base := strings.TrimSuffix(p, "*")
		lowerBase := strings.ToLower(base)
		check := base
		hasSubWildcard := strings.HasPrefix(lowerBase, "https://*.")
		if hasSubWildcard {
			check = "https://wildcard." + base[len("https://*."):]
		}
		u, err := url.Parse(check)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return fmt.Errorf("invalid redirect URI wildcard: %s", p)
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return fmt.Errorf("redirect URI wildcard must use https: %s", p)
		}
		if hasSubWildcard {
			domain := strings.TrimPrefix(strings.ToLower(u.Hostname()), "wildcard.")
			if domain == "" || strings.Contains(domain, "*") || strings.Count(domain, ".") < 1 {
				return fmt.Errorf("redirect URI host wildcard requires a registrable domain: %s", p)
			}
		}
		if !strings.HasSuffix(u.Path, "/") || hasParentSegment(u.Path) {
			return fmt.Errorf("redirect URI wildcard must end in a clean path prefix: %s", p)
		}
		return nil
	}
	if _, ok := validRedirectCandidate(p); !ok {
		return fmt.Errorf("invalid redirect URI: %s", p)
	}
	return nil
}

func normalizedHTTPSPort(port string) string {
	if port == "443" {
		return ""
	}
	return port
}

// matchRedirectPattern reports whether candidate matches one configured
// pattern. The candidate is always validated first, so a malformed or
// non-TLS (non-loopback) callback can never match.
func matchRedirectPattern(candidate, pattern string) bool {
	cu, ok := validRedirectCandidate(candidate)
	if !ok {
		return false
	}
	p := strings.TrimSpace(pattern)
	switch p {
	case "https://*":
		return strings.EqualFold(cu.Scheme, "https")
	case oauthLoopbackPattern127, oauthLoopbackPatternLocalhost:
		if !strings.EqualFold(cu.Scheme, "http") {
			return false
		}
		host := strings.ToLower(cu.Hostname())
		if p == oauthLoopbackPattern127 {
			return host == "127.0.0.1"
		}
		return host == "localhost"
	}
	if strings.HasSuffix(p, "/*") {
		prefix := strings.TrimSuffix(p, "*")
		wildcardSub := strings.HasPrefix(strings.ToLower(prefix), "https://*.")
		parseTarget := prefix
		if wildcardSub {
			parseTarget = "https://wildcard." + prefix[len("https://*."):]
		}
		pu, err := url.Parse(parseTarget)
		if err != nil || pu.Host == "" || !strings.EqualFold(cu.Scheme, "https") {
			return false
		}
		if wildcardSub {
			baseDomain := strings.ToLower(strings.TrimPrefix(pu.Hostname(), "wildcard."))
			gotHost := strings.ToLower(cu.Hostname())
			if !strings.HasSuffix(gotHost, "."+baseDomain) || normalizedHTTPSPort(cu.Port()) != normalizedHTTPSPort(pu.Port()) {
				return false
			}
		} else if !strings.EqualFold(cu.Hostname(), pu.Hostname()) || normalizedHTTPSPort(cu.Port()) != normalizedHTTPSPort(pu.Port()) {
			return false
		}
		return cu.Path == pu.Path || strings.HasPrefix(cu.Path, pu.Path)
	}
	pu, ok := validRedirectCandidate(p)
	if !ok {
		return false
	}
	return strings.EqualFold(cu.Scheme, pu.Scheme) &&
		strings.EqualFold(cu.Host, pu.Host) &&
		cu.Path == pu.Path &&
		cu.RawQuery == pu.RawQuery
}

func matchesAnyRedirectPattern(candidate string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchRedirectPattern(candidate, pattern) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Scope and PKCE helpers

func normalizeScopes(raw string) []string {
	scopes := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, s := range strings.Fields(raw) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		scopes = append(scopes, s)
	}
	if len(scopes) == 0 {
		return []string{"mcp"}
	}
	if !seen["mcp"] {
		scopes = append(scopes, "mcp")
	}
	return scopes
}

func validCodeChallenge(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func validCodeVerifier(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			continue
		}
		return false
	}
	return true
}

func pkceChallengeForVerifier(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Pending codes and consent tickets

func (o *oauthServer) mintCode(clientID, redirectURI, challenge, resource string, scopes []string) string {
	now := time.Now()
	code := "amg_ac_" + randomHex(32)
	o.mu.Lock()
	for key, c := range o.codes {
		if now.After(c.ExpiresAt) {
			delete(o.codes, key)
		}
	}
	if len(o.codes) >= oauthMaxCodes {
		var oldestKey string
		var oldest time.Time
		for key, c := range o.codes {
			if oldestKey == "" || c.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, c.CreatedAt
			}
		}
		delete(o.codes, oldestKey)
	}
	o.codes[code] = &oauthCode{
		Code: code, ClientID: clientID, RedirectURI: redirectURI, Scopes: scopes,
		CodeChallenge: challenge, Resource: resource, CreatedAt: now, ExpiresAt: now.Add(oauthCodeTTL),
	}
	o.mu.Unlock()
	return code
}

func (o *oauthServer) consumeCode(raw string) (*oauthCode, bool) {
	if raw == "" {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	c, ok := o.codes[raw]
	if !ok {
		return nil, false
	}
	delete(o.codes, raw)
	if time.Now().After(c.ExpiresAt) {
		return nil, false
	}
	return c, true
}

func (o *oauthServer) mintTicket(t *oauthTicket) string {
	token := randomHex(32)
	now := time.Now()
	t.CreatedAt = now
	t.ExpiresAt = now.Add(oauthTicketTTL)
	o.mu.Lock()
	for key, existing := range o.tickets {
		if now.After(existing.ExpiresAt) {
			delete(o.tickets, key)
		}
	}
	if len(o.tickets) >= oauthMaxTickets {
		var oldestKey string
		var oldest time.Time
		for key, existing := range o.tickets {
			if oldestKey == "" || existing.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, existing.CreatedAt
			}
		}
		delete(o.tickets, oldestKey)
	}
	o.tickets[token] = t
	o.mu.Unlock()
	return token
}

func (o *oauthServer) consumeTicket(raw string) (*oauthTicket, bool) {
	if raw == "" {
		return nil, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	t, ok := o.tickets[raw]
	if !ok {
		return nil, false
	}
	delete(o.tickets, raw)
	if time.Now().After(t.ExpiresAt) {
		return nil, false
	}
	return t, true
}

// ---------------------------------------------------------------------------
// Token lifecycle

// verifyAccessToken resolves a plaintext access token without persisting the
// plaintext; only its digest is compared.
func (o *oauthServer) verifyAccessToken(raw string) (*oauthAccessToken, error) {
	if !strings.HasPrefix(raw, "amg_at_") {
		return nil, auth.ErrInvalidToken
	}
	hash := sha256Hex(raw)
	now := time.Now()
	o.mu.Lock()
	changed := o.sweepLocked(now)
	var found *oauthAccessToken
	for i := range o.state.AccessTokens {
		if digestEqual(o.state.AccessTokens[i].TokenSHA256, hash) {
			rec := o.state.AccessTokens[i]
			found = &rec
			break
		}
	}
	if changed {
		_ = o.saveLocked()
	}
	o.mu.Unlock()
	if found == nil || !now.Before(found.ExpiresAt) {
		return nil, auth.ErrInvalidToken
	}
	return found, nil
}

func (o *oauthServer) issueTokens(clientID string, scopes []string) (string, string, int, error) {
	c := o.app.config.Get()
	ttlSec := 30 * 86400
	refreshDays := 90
	if c.OAuth != nil && c.OAuth.AccessTokenTTLSec > 0 {
		ttlSec = c.OAuth.AccessTokenTTLSec
	}
	if c.OAuth != nil && c.OAuth.RefreshTokenTTLDays > 0 {
		refreshDays = c.OAuth.RefreshTokenTTLDays
	}
	now := time.Now()
	rawAT := "amg_at_" + randomHex(32)
	rawRT := "amg_rt_" + randomHex(32)
	rtHash := sha256Hex(rawRT)

	o.mu.Lock()
	o.sweepLocked(now)
	if len(o.state.RefreshTokens) >= oauthMaxRefreshTokens {
		o.evictOldestRefreshLocked()
	}
	o.state.RefreshTokens = append(o.state.RefreshTokens, oauthRefreshToken{
		TokenSHA256: rtHash, ClientID: clientID, Scopes: scopes,
		ExpiresAt: now.Add(time.Duration(refreshDays) * 24 * time.Hour), CreatedAt: now,
	})
	if len(o.state.AccessTokens) >= oauthMaxAccessTokens {
		o.evictOldestAccessLocked()
	}
	o.state.AccessTokens = append(o.state.AccessTokens, oauthAccessToken{
		TokenSHA256: sha256Hex(rawAT), ClientID: clientID, Scopes: scopes,
		ExpiresAt: now.Add(time.Duration(ttlSec) * time.Second), CreatedAt: now,
		RefreshTokenSHA256: rtHash,
	})
	err := o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		return "", "", 0, err
	}
	return rawAT, rawRT, ttlSec, nil
}

// refreshAccessToken extends the lifetime of the existing refresh token and
// mints a new access token for it. The refresh token itself is never rotated.
func (o *oauthServer) refreshAccessToken(rawRT, clientID string) (string, []string, int, error) {
	c := o.app.config.Get()
	ttlSec := 30 * 86400
	refreshDays := 90
	if c.OAuth != nil && c.OAuth.AccessTokenTTLSec > 0 {
		ttlSec = c.OAuth.AccessTokenTTLSec
	}
	if c.OAuth != nil && c.OAuth.RefreshTokenTTLDays > 0 {
		refreshDays = c.OAuth.RefreshTokenTTLDays
	}
	now := time.Now()
	hash := sha256Hex(rawRT)
	rawAT := "amg_at_" + randomHex(32)

	o.mu.Lock()
	o.sweepLocked(now)
	matched := -1
	for i := range o.state.RefreshTokens {
		if digestEqual(o.state.RefreshTokens[i].TokenSHA256, hash) && o.state.RefreshTokens[i].ClientID == clientID {
			matched = i
			break
		}
	}
	if matched < 0 {
		o.mu.Unlock()
		return "", nil, 0, errInvalidGrant
	}
	o.state.RefreshTokens[matched].ExpiresAt = now.Add(time.Duration(refreshDays) * 24 * time.Hour)
	scopes := append([]string(nil), o.state.RefreshTokens[matched].Scopes...)
	if len(o.state.AccessTokens) >= oauthMaxAccessTokens {
		o.evictOldestAccessLocked()
	}
	o.state.AccessTokens = append(o.state.AccessTokens, oauthAccessToken{
		TokenSHA256: sha256Hex(rawAT), ClientID: clientID, Scopes: scopes,
		ExpiresAt: now.Add(time.Duration(ttlSec) * time.Second), CreatedAt: now,
		RefreshTokenSHA256: hash,
	})
	err := o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		return "", nil, 0, err
	}
	return rawAT, scopes, ttlSec, nil
}

func (o *oauthServer) revokeToken(clientID, raw string) error {
	if raw == "" {
		return nil
	}
	hash := sha256Hex(raw)
	o.mu.Lock()
	defer o.mu.Unlock()
	changed := false

	access := make([]oauthAccessToken, 0, len(o.state.AccessTokens))
	for _, t := range o.state.AccessTokens {
		if t.TokenSHA256 == hash && (clientID == "" || t.ClientID == clientID) {
			changed = true
			continue
		}
		access = append(access, t)
	}
	o.state.AccessTokens = access

	refresh := make([]oauthRefreshToken, 0, len(o.state.RefreshTokens))
	revokedRefresh := false
	for _, t := range o.state.RefreshTokens {
		if t.TokenSHA256 == hash && (clientID == "" || t.ClientID == clientID) {
			changed = true
			revokedRefresh = true
			continue
		}
		refresh = append(refresh, t)
	}
	o.state.RefreshTokens = refresh
	if revokedRefresh {
		kept := make([]oauthAccessToken, 0, len(o.state.AccessTokens))
		for _, t := range o.state.AccessTokens {
			if t.RefreshTokenSHA256 == hash {
				changed = true
				continue
			}
			kept = append(kept, t)
		}
		o.state.AccessTokens = kept
	}
	if !changed {
		return nil
	}
	return o.saveLocked()
}

func (o *oauthServer) evictOldestAccessLocked() {
	if len(o.state.AccessTokens) == 0 {
		return
	}
	oldest := 0
	for i := range o.state.AccessTokens {
		if o.state.AccessTokens[i].CreatedAt.Before(o.state.AccessTokens[oldest].CreatedAt) {
			oldest = i
		}
	}
	o.state.AccessTokens = append(o.state.AccessTokens[:oldest], o.state.AccessTokens[oldest+1:]...)
}

func (o *oauthServer) evictOldestRefreshLocked() {
	if len(o.state.RefreshTokens) == 0 {
		return
	}
	oldest := 0
	for i := range o.state.RefreshTokens {
		if o.state.RefreshTokens[i].CreatedAt.Before(o.state.RefreshTokens[oldest].CreatedAt) {
			oldest = i
		}
	}
	dropped := o.state.RefreshTokens[oldest].TokenSHA256
	o.state.RefreshTokens = append(o.state.RefreshTokens[:oldest], o.state.RefreshTokens[oldest+1:]...)
	kept := make([]oauthAccessToken, 0, len(o.state.AccessTokens))
	for _, t := range o.state.AccessTokens {
		if t.RefreshTokenSHA256 != dropped {
			kept = append(kept, t)
		}
	}
	o.state.AccessTokens = kept
}

// ---------------------------------------------------------------------------
// HTTP endpoints

func (a *App) registerOAuth(mux *http.ServeMux) {
	o := a.oauth
	if o == nil {
		return
	}
	base := o.publicBase()
	prmStateful := allowHEAD(auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               base + "/mcp",
		AuthorizationServers:   []string{base},
		ScopesSupported:        []string{"mcp"},
		BearerMethodsSupported: []string{"header"},
	}))
	prmStateless := allowHEAD(auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               base + "/mcp/stateless",
		AuthorizationServers:   []string{base},
		ScopesSupported:        []string{"mcp"},
		BearerMethodsSupported: []string{"header"},
	}))
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		mux.Handle(path, a.oauthHostGuard("prm", prmStateful))
	}
	mux.Handle("/.well-known/oauth-protected-resource/mcp/stateless", a.oauthHostGuard("prm_stateless", prmStateless))
	for _, path := range []string{
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/mcp",
		"/.well-known/openid-configuration",
		"/.well-known/openid-configuration/mcp",
	} {
		mux.Handle(path, a.oauthHostGuard("as_meta", allowHEAD(http.HandlerFunc(o.handleAuthServerMetadata))))
	}
	mux.Handle("/oauth/register", a.oauthHostGuard("register", http.HandlerFunc(o.handleRegister)))
	mux.Handle("/oauth/authorize", a.oauthHostGuard("authorize", http.HandlerFunc(o.handleAuthorize)))
	mux.Handle("/oauth/token", a.oauthHostGuard("token", http.HandlerFunc(o.handleToken)))
	mux.Handle("/oauth/revoke", a.oauthHostGuard("revoke", http.HandlerFunc(o.handleRevoke)))
}

type headOnlyWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *headOnlyWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *headOnlyWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return len(p), nil
}

func (w *headOnlyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// allowHEAD serves HTTP HEAD against a GET metadata handler with identical
// status and headers and an empty body, matching Connected Apps probes.
func allowHEAD(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		clone := r.Clone(r.Context())
		clone.Method = http.MethodGet
		hw := &headOnlyWriter{ResponseWriter: w}
		next.ServeHTTP(hw, clone)
		if !hw.wroteHeader {
			hw.WriteHeader(http.StatusOK)
		}
	})
}

type statusCaptureWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCaptureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCaptureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// emitOAuthProbe logs only server-owned route/method/status constants (never
// URIs, query strings, headers, client names, or tokens).
func (a *App) emitOAuthProbe(route, method string, status int, reason string) {
	if a.requestLogger == nil {
		return
	}
	safeMethod := "OTHER"
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions:
		safeMethod = method
	}
	entry := struct {
		Event  string `json:"event"`
		Route  string `json:"route"`
		Method string `json:"method"`
		Status int    `json:"status"`
		Reason string `json:"reason,omitempty"`
	}{"amg_oauth", route, safeMethod, status, reason}
	encoded, _ := json.Marshal(entry)
	a.requestLogger.Print(string(encoded))
}

func (a *App) oauthHostGuard(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.checkHost(w, r) {
			a.emitOAuthProbe(route, r.Method, http.StatusForbidden, "host_denied")
			return
		}
		sw := &statusCaptureWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		a.emitOAuthProbe(route, r.Method, status, "")
	})
}

func (o *oauthServer) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w, "GET, HEAD, OPTIONS", "Content-Type, Authorization, MCP-Protocol-Version")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	base := o.publicBase()
	meta := oauthex.AuthServerMeta{
		Issuer:                            base,
		AuthorizationEndpoint:             base + "/oauth/authorize",
		TokenEndpoint:                     base + "/oauth/token",
		RegistrationEndpoint:              base + "/oauth/register",
		RevocationEndpoint:                base + "/oauth/revoke",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_post", "client_secret_basic"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		ScopesSupported:                   []string{"mcp", "openid", "profile", "email"},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(meta)
}

func (o *oauthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w, "GET, POST, OPTIONS", "Content-Type, Authorization")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	c := o.app.config.Get()
	if c.OAuth != nil && c.OAuth.AllowDynamicReg != nil && !*c.OAuth.AllowDynamicReg {
		o.app.emitOAuthProbe("register_check", r.Method, http.StatusForbidden, "dcr_disabled")
		writeOAuthError(w, http.StatusForbidden, "invalid_client_metadata", "dynamic client registration is disabled")
		return
	}
	if !o.registerLimiter.Allow() {
		o.app.emitOAuthProbe("register_check", r.Method, http.StatusTooManyRequests, "rate_limited")
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many registration attempts")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBodyTokens)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		o.app.emitOAuthProbe("register_check", r.Method, http.StatusBadRequest, "body_unreadable")
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body is too large or unreadable")
		return
	}
	var meta oauthex.ClientRegistrationMetadata
	if err := json.Unmarshal(body, &meta); err != nil {
		o.app.emitOAuthProbe("register_check", r.Method, http.StatusBadRequest, "invalid_json")
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body must be a JSON object")
		return
	}
	if len(meta.RedirectURIs) == 0 || len(meta.RedirectURIs) > 16 {
		o.app.emitOAuthProbe("register_check", r.Method, http.StatusBadRequest, "redirect_count")
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "one to sixteen redirect_uris are required")
		return
	}
	allowlist := c.OAuth.AllowedRedirectURIs
	if len(allowlist) == 0 {
		allowlist = DefaultOAuthRedirectPatterns()
	}
	for _, uri := range meta.RedirectURIs {
		if _, ok := validRedirectCandidate(uri); !ok {
			o.app.emitOAuthProbe("register_check", r.Method, http.StatusBadRequest, "invalid_redirect_candidate")
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be a clean https URL or http loopback URL")
			return
		}
		if !matchesAnyRedirectPattern(uri, allowlist) {
			o.app.emitOAuthProbe("register_check", r.Method, http.StatusBadRequest, "redirect_not_in_allowlist")
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri is not allowed: "+uri)
			return
		}
	}
	method := strings.TrimSpace(meta.TokenEndpointAuthMethod)
	if method == "" {
		method = "none"
	}
	switch method {
	case "none", "client_secret_post", "client_secret_basic":
	default:
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}

	clientID := "amg_cid_" + randomHex(16)
	record := oauthClientRecord{
		ClientID:                clientID,
		ClientName:              meta.ClientName,
		RedirectURIs:            append([]string(nil), meta.RedirectURIs...),
		TokenEndpointAuthMethod: method,
		Dynamic:                 true,
		CreatedAt:               time.Now(),
	}
	var rawSecret string
	if method != "none" {
		rawSecret = "amg_cs_" + randomHex(32)
		record.ClientSecretSHA256 = sha256Hex(rawSecret)
	}

	o.mu.Lock()
	if len(o.state.Clients) >= oauthMaxDynamicClients {
		oldest := 0
		for i := range o.state.Clients {
			if o.state.Clients[i].CreatedAt.Before(o.state.Clients[oldest].CreatedAt) {
				oldest = i
			}
		}
		o.state.Clients = append(o.state.Clients[:oldest], o.state.Clients[oldest+1:]...)
	}
	o.state.Clients = append(o.state.Clients, record)
	err = o.saveLocked()
	o.mu.Unlock()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist the client registration")
		return
	}

	response := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        record.CreatedAt.Unix(),
		"redirect_uris":              record.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
	}
	if record.ClientName != "" {
		response["client_name"] = record.ClientName
	}
	if rawSecret != "" {
		response["client_secret"] = rawSecret
		response["client_secret_expires_at"] = 0
	}
	writeJSON(w, http.StatusCreated, response)
}

func (o *oauthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	setConsentHeaders(w)
	switch r.Method {
	case http.MethodGet:
		o.authorizeGET(w, r)
	case http.MethodPost:
		o.authorizePOST(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func setConsentHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

func (o *oauthServer) authorizeGET(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := strings.TrimSpace(q.Get("client_id"))
	redirectURI := q.Get("redirect_uri")

	client, ok := o.client(clientID)
	if !ok {
		http.Error(w, "unknown client_id", http.StatusBadRequest)
		return
	}
	if !o.redirectAllowed(client, redirectURI) {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" {
		o.redirectOAuthError(w, r, redirectURI, "invalid_request", "response_type must be code")
		return
	}
	challenge := q.Get("code_challenge")
	challengeMethod := q.Get("code_challenge_method")
	if client.ClientSecretSHA256 == "" || challenge != "" || challengeMethod != "" {
		if challengeMethod != "S256" || !validCodeChallenge(challenge) {
			o.redirectOAuthError(w, r, redirectURI, "invalid_request", "a valid S256 code_challenge is required")
			return
		}
	}
	resource := strings.TrimSpace(q.Get("resource"))
	if !o.resourceAllowed(resource) {
		o.redirectOAuthError(w, r, redirectURI, "invalid_target", "resource is not served by this authorization server")
		return
	}
	scopes := normalizeScopes(q.Get("scope"))
	if !o.ticketLimiter.Allow() {
		http.Error(w, "too many authorization requests", http.StatusTooManyRequests)
		return
	}
	ticket := &oauthTicket{
		ClientID: clientID, RedirectURI: redirectURI, State: q.Get("state"),
		Scopes: scopes, ScopeStr: strings.Join(scopes, " "),
		CodeChallenge: challenge, Resource: resource,
	}
	token := o.mintTicket(ticket)
	o.renderConsent(w, http.StatusOK, ticket, token, clientDisplayName(client), "")
}

func (o *oauthServer) authorizePOST(w http.ResponseWriter, r *http.Request) {
	if origins := r.Header.Values("Origin"); len(origins) > 1 {
		http.Error(w, "origin denied", http.StatusForbidden)
		return
	} else if len(origins) == 1 && !o.originAllowed(origins[0]) {
		http.Error(w, "origin denied", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBodySmall)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form body", http.StatusBadRequest)
		return
	}
	ticket, ok := o.consumeTicket(r.PostFormValue("csrf_token"))
	if !ok {
		http.Error(w, "invalid or expired authorization session", http.StatusBadRequest)
		return
	}
	if !ticketMatchesForm(ticket, r) {
		http.Error(w, "invalid or expired authorization session", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("decision") == "deny" {
		o.redirectOAuthError(w, r, ticket.RedirectURI, "access_denied", "the resource owner denied the request")
		return
	}
	client, ok := o.client(ticket.ClientID)
	if !ok {
		http.Error(w, "invalid or expired authorization session", http.StatusBadRequest)
		return
	}
	if !o.passkeyLimiter.Allow() {
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	passkey := strings.TrimSpace(r.PostFormValue("passkey"))
	if !tokenEqual(passkey, o.app.token) {
		token := o.mintTicket(ticket)
		o.renderConsent(w, http.StatusUnauthorized, ticket, token, clientDisplayName(client), "Incorrect owner passkey. Try again.")
		return
	}
	code := o.mintCode(ticket.ClientID, ticket.RedirectURI, ticket.CodeChallenge, ticket.Resource, ticket.Scopes)
	redirectOAuthCode(w, r, ticket.RedirectURI, code, o.publicBase(), ticket.State)
}

func ticketMatchesForm(ticket *oauthTicket, r *http.Request) bool {
	if r.PostFormValue("client_id") != ticket.ClientID {
		return false
	}
	if r.PostFormValue("redirect_uri") != ticket.RedirectURI {
		return false
	}
	if r.PostFormValue("state") != ticket.State {
		return false
	}
	if r.PostFormValue("code_challenge") != ticket.CodeChallenge {
		return false
	}
	if r.PostFormValue("resource") != ticket.Resource {
		return false
	}
	return normalizeScopeString(r.PostFormValue("scope")) == ticket.ScopeStr
}

func normalizeScopeString(raw string) string {
	return strings.Join(normalizeScopes(raw), " ")
}

func clientDisplayName(client oauthClientRecord) string {
	if name := strings.TrimSpace(client.ClientName); name != "" {
		return name
	}
	return client.ClientID
}

func redirectOAuthCode(w http.ResponseWriter, r *http.Request, rawRedirect, code, issuer, state string) {
	u, err := url.Parse(rawRedirect)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	query := u.Query()
	query.Set("code", code)
	query.Set("iss", issuer)
	if state != "" {
		query.Set("state", state)
	}
	u.RawQuery = query.Encode()
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusSeeOther)
}

func (o *oauthServer) redirectOAuthError(w http.ResponseWriter, r *http.Request, rawRedirect, code, description string) {
	u, err := url.Parse(rawRedirect)
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	query := u.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state := r.FormValue("state"); state != "" {
		query.Set("state", state)
	} else if state := r.URL.Query().Get("state"); state != "" {
		query.Set("state", state)
	}
	query.Set("iss", o.publicBase())
	u.RawQuery = query.Encode()
	w.Header().Set("Location", u.String())
	w.WriteHeader(http.StatusSeeOther)
}

func (o *oauthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w, "POST, OPTIONS", "Content-Type, Authorization, MCP-Protocol-Version")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBodyTokens)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	clientID, clientSecret, usedBasic, credentialsOK := tokenClientCredentials(r)
	if !credentialsOK {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid client authentication")
		return
	}
	client, ok := o.client(clientID)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if client.ClientSecretSHA256 != "" {
		if !secretMatches(client.ClientSecretSHA256, clientSecret) {
			if usedBasic {
				w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
			}
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		o.handleAuthorizationCodeGrant(w, r, clientID)
	case "refresh_token":
		o.handleRefreshGrant(w, r, clientID)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (o *oauthServer) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request, clientID string) {
	rec, ok := o.consumeCode(r.PostFormValue("code"))
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if rec.ClientID != clientID || rec.RedirectURI != r.PostFormValue("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code does not belong to this client and redirect_uri")
		return
	}
	verifier := r.PostFormValue("code_verifier")
	if rec.CodeChallenge != "" || verifier != "" {
		if rec.CodeChallenge == "" || !validCodeVerifier(verifier) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "a valid PKCE code_verifier is required")
			return
		}
		expected := pkceChallengeForVerifier(verifier)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(rec.CodeChallenge)) != 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
	}
	rawAT, rawRT, ttlSec, err := o.issueTokens(clientID, rec.Scopes)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist issued tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": rawAT, "token_type": "Bearer", "expires_in": ttlSec,
		"refresh_token": rawRT, "scope": strings.Join(rec.Scopes, " "),
	})
}

func (o *oauthServer) handleRefreshGrant(w http.ResponseWriter, r *http.Request, clientID string) {
	rawRT := r.PostFormValue("refresh_token")
	if rawRT == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token is required")
		return
	}
	rawAT, scopes, ttlSec, err := o.refreshAccessToken(rawRT, clientID)
	if errors.Is(err, errInvalidGrant) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh_token is invalid or expired")
		return
	}
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist refreshed tokens")
		return
	}
	// Gemini requires the refresh token to stay stable across refreshes.
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": rawAT, "token_type": "Bearer", "expires_in": ttlSec,
		"refresh_token": rawRT, "scope": strings.Join(scopes, " "),
	})
}

func (o *oauthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w, "POST, OPTIONS", "Content-Type, Authorization")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOAuthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBodySmall)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	clientID := strings.TrimSpace(r.PostFormValue("client_id"))
	id, secret, usedBasic, credentialsOK := tokenClientCredentials(r)
	if id != "" {
		clientID = id
	}
	if len(r.Header.Values("Authorization")) > 0 && !credentialsOK {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "invalid client authentication")
		return
	}
	if clientID == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
		return
	}
	client, ok := o.client(clientID)
	if !ok {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}
	if client.ClientSecretSHA256 != "" {
		if !credentialsOK || !secretMatches(client.ClientSecretSHA256, secret) {
			if usedBasic {
				w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
			}
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}
	}
	if err := o.revokeToken(clientID, r.PostFormValue("token")); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist revocation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func setOAuthCORS(w http.ResponseWriter, methods, headers string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", methods)
	w.Header().Set("Access-Control-Allow-Headers", headers)
}

// ---------------------------------------------------------------------------
// Client authentication helpers

// tokenClientCredentials extracts client credentials from HTTP Basic
// (RFC 6749 §2.3.1, form-urlencoded before base64) or the POST form body.
// Sending both a Basic header and a form client_secret is rejected.
func tokenClientCredentials(r *http.Request) (clientID, clientSecret string, usedBasic, ok bool) {
	formSecret := r.PostFormValue("client_secret")
	formID := strings.TrimSpace(r.PostFormValue("client_id"))
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(header), "basic ") {
		usedBasic = true
		raw := strings.TrimSpace(header[len("basic "):])
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return "", "", usedBasic, false
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) != 2 {
			return "", "", usedBasic, false
		}
		id, secret := parts[0], parts[1]
		if v, err := url.QueryUnescape(id); err == nil {
			id = v
		}
		if v, err := url.QueryUnescape(secret); err == nil {
			secret = v
		}
		if formSecret != "" {
			return "", "", usedBasic, false
		}
		if formID != "" && formID != id {
			return "", "", usedBasic, false
		}
		return id, secret, usedBasic, id != ""
	}
	return formID, formSecret, false, formID != ""
}

func digestEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func secretMatches(expectedSHA256, raw string) bool {
	if raw == "" || expectedSHA256 == "" {
		return false
	}
	return digestEqual(sha256Hex(raw), expectedSHA256)
}

var errInvalidGrant = errors.New("invalid_grant")

// ---------------------------------------------------------------------------
// JSON and consent rendering

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

type oauthConsentView struct {
	ClientName    string
	ClientID      string
	RedirectURI   string
	Scopes        string
	CSRFToken     string
	State         string
	CodeChallenge string
	Resource      string
	Error         string
}

func (o *oauthServer) renderConsent(w http.ResponseWriter, status int, ticket *oauthTicket, csrf, clientName, failure string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	view := oauthConsentView{
		ClientName: clientName, ClientID: ticket.ClientID, RedirectURI: ticket.RedirectURI,
		Scopes: ticket.ScopeStr, CSRFToken: csrf, State: ticket.State,
		CodeChallenge: ticket.CodeChallenge, Resource: ticket.Resource, Error: failure,
	}
	if err := oauthConsentTemplate.Execute(w, view); err != nil {
		// The status line and headers are already on the wire; nothing else
		// can be reported without corrupting the page.
		return
	}
}

var oauthConsentTemplate = template.Must(template.New("consent").Parse(oauthConsentHTML))

const oauthConsentHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize access</title>
<style>
:root { color-scheme: light dark; --ink: #14161a; --muted: #5c6470; --line: #d9dee6; --bg: #f6f7f9; --card: #ffffff; --accent: #2563eb; }
@media (prefers-color-scheme: dark) { :root { --ink: #e8eaee; --muted: #a2aab6; --line: #2c323c; --bg: #11141a; --card: #1a1e25; --accent: #6d9bff; } }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 24px; background: var(--bg); color: var(--ink); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { width: 100%; max-width: 480px; background: var(--card); border: 1px solid var(--line); border-radius: 16px; padding: 28px; box-shadow: 0 10px 30px rgb(0 0 0 / 0.08); }
h1 { margin: 0 0 8px; font-size: 1.35rem; }
p { margin: 0 0 16px; }
.lede { color: var(--muted); }
.error { border: 1px solid #d14343; background: rgb(209 67 67 / 0.1); color: #b3261e; padding: 10px 12px; border-radius: 10px; }
dl { display: grid; grid-template-columns: auto 1fr; gap: 8px 16px; margin: 0 0 20px; font-size: 0.92rem; }
dt { color: var(--muted); }
dd { margin: 0; overflow-wrap: anywhere; }
label { display: block; font-weight: 600; margin-bottom: 6px; }
input[type=password] { width: 100%; padding: 12px; border: 1px solid var(--line); border-radius: 10px; background: transparent; color: inherit; font: inherit; }
.actions { display: flex; gap: 12px; margin-top: 20px; }
button { flex: 1; padding: 12px 16px; border-radius: 10px; border: 1px solid var(--accent); background: var(--accent); color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
button.deny { background: transparent; color: var(--ink); border-color: var(--line); }
</style>
</head>
<body>
<main>
<h1>Authorize access</h1>
<p class="lede"><strong>{{.ClientName}}</strong> is requesting access to this workspace.</p>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
<dl>
<dt>Client ID</dt><dd>{{.ClientID}}</dd>
<dt>Callback</dt><dd>{{.RedirectURI}}</dd>
<dt>Scopes</dt><dd>{{.Scopes}}</dd>
</dl>
<form method="post" action="/oauth/authorize">
<input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="scope" value="{{.Scopes}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<input type="hidden" name="resource" value="{{.Resource}}">
<label for="passkey">Owner passkey</label>
<input id="passkey" name="passkey" type="password" autocomplete="off" data-1p-ignore="true" data-lpignore="true" required autofocus>
<div class="actions">
<button type="submit" name="decision" value="authorize">Authorize</button>
<button type="submit" name="decision" value="deny" class="deny" formnovalidate>Deny</button>
</div>
</form>
</main>
</body>
</html>
`
