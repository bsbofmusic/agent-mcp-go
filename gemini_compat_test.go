package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// geminiCompatTestApp mirrors testApp but exposes the two configuration knobs
// exercised by host-compatibility behavior: annotation coverage and the
// browser origin allowlist. Keeping it local avoids touching the shared
// testApp helper that many unrelated tests rely on.
func geminiCompatTestApp(t *testing.T, annotationsMode string, allowedOrigins []string) (*App, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	token := strings.Repeat("g", 48)
	tokPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Listen: "127.0.0.1:3100", WorkRoot: dir, StateDir: filepath.Join(dir, "state"),
		SkillsRoot: filepath.Join(dir, "skills"), TokenFile: tokPath, PublicURL: "",
		AllowedOrigins:  append([]string(nil), allowedOrigins...),
		AnnotationsMode: annotationsMode,
		MaxFileBytes:    300 * 1024 * 1024, MaxOutputBytes: 128 * 1024, CommandTimeoutSec: 60,
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.handler())
	t.Cleanup(srv.Close)
	return app, srv, token
}

// registeredTools reads the live tool registry through the stateless MCP
// transport, which is what browser-side hosts consume.
func registeredTools(t *testing.T, srv *httptest.Server, token string) []map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp/stateless",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:3100"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status %d: %s", resp.StatusCode, body)
	}
	var parsed struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode tools/list response: %v (body %q)", err, body)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Fatal("tools/list returned an empty registry")
	}
	return parsed.Result.Tools
}

// TestToolDescriptionsAvoidPolicyViolations guards the hosted-model policy
// surface: descriptions must stay declarative, must not script a multi-call
// behavior or shout emphasis, must not advertise unrestricted capability, and
// must not name other tools.
func TestToolDescriptionsAvoidPolicyViolations(t *testing.T) {
	_, srv, token := geminiCompatTestApp(t, "", nil)
	tools := registeredTools(t, srv, token)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	instructionPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)call [a-z_]+ first`),
		regexp.MustCompile(`(?i)start here`),
		regexp.MustCompile(`(?i)\buse [a-z_]+ (for|to)\b`),
		regexp.MustCompile(`(?i)before (you|changing)`),
	}
	emphasisPattern := regexp.MustCompile(`\b(MUST|NEVER|ALWAYS|IMPORTANT|CRITICAL|REQUIRED|DO NOT)\b`)
	riskyPattern := regexp.MustCompile(`(?i)\b(unrestricted|arbitrary|bypass|any system command|elevated)\b`)
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		if strings.TrimSpace(description) == "" {
			t.Errorf("%s: empty description", name)
			continue
		}
		for _, other := range names {
			if other == "" || other == name {
				continue
			}
			reference := regexp.MustCompile(`\b` + regexp.QuoteMeta(other) + `\b`)
			if match := reference.FindString(description); match != "" {
				t.Errorf("%s: description names another tool (%q): %q", name, match, description)
			}
		}
		for _, pattern := range instructionPatterns {
			if match := pattern.FindString(description); match != "" {
				t.Errorf("%s: description scripts behavior (%q): %q", name, match, description)
			}
		}
		if match := emphasisPattern.FindString(description); match != "" {
			t.Errorf("%s: description uses shouting emphasis (%q): %q", name, match, description)
		}
		if match := riskyPattern.FindString(description); match != "" {
			t.Errorf("%s: description advertises risky capability (%q): %q", name, match, description)
		}
	}
}

// TestEveryToolHasTitleAndFullAnnotations checks the display surface that
// browser hosts render: every tool carries a title and all four annotation
// hints. The trusted annotation mode is the one that promises complete hints
// for the whole catalogue.
func TestEveryToolHasTitleAndFullAnnotations(t *testing.T) {
	_, srv, token := geminiCompatTestApp(t, "trusted", nil)
	tools := registeredTools(t, srv, token)
	if len(tools) != 40 {
		t.Fatalf("expected the 40-tool AMG catalogue, got %d", len(tools))
	}
	hints := []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		title, _ := tool["title"].(string)
		if strings.TrimSpace(title) == "" {
			t.Errorf("%s: missing display title", name)
		}
		annotations, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Errorf("%s: missing annotations object", name)
			continue
		}
		for _, hint := range hints {
			if _, present := annotations[hint]; !present {
				t.Errorf("%s: annotation %s is absent", name, hint)
			}
		}
	}
}

// TestDefaultModeDeclaresAllAnnotations pins the honest default mode: every
// tool in the catalogue resolves its own safety hints, with no tool falling
// back to implicit client defaults. Trusted mode synthesizes hints for the whole
// catalogue, so only this mode would expose a missing per-tool mapping.
func TestDefaultModeDeclaresAllAnnotations(t *testing.T) {
	_, srv, token := geminiCompatTestApp(t, "", nil)
	tools := registeredTools(t, srv, token)
	if len(tools) != 40 {
		t.Fatalf("expected the 40-tool AMG catalogue, got %d", len(tools))
	}
	hints := []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		annotations, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Errorf("%s: missing annotations object in default mode", name)
			continue
		}
		for _, hint := range hints {
			if _, present := annotations[hint]; !present {
				t.Errorf("%s: annotation %s is absent in default mode", name, hint)
			}
		}
	}
}

// TestCORSHeadersFollowAllowedOrigins pins the browser transport contract:
// preflight succeeds only for an allowlisted origin, actual responses expose
// the MCP headers, credentials are never allowed, and an empty allowlist keeps
// the previous behavior exactly.
func TestCORSHeadersFollowAllowedOrigins(t *testing.T) {
	const geminiOrigin = "https://gemini.google.com"
	const toolsListBody = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	_, srv, token := geminiCompatTestApp(t, "", []string{geminiOrigin, "https://claude.ai"})

	request := func(target *httptest.Server, method, path, origin, bearer, body string) (*http.Response, string) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, target.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:3100"
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json, text/event-stream")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := target.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp, string(got)
	}
	noAccessControl := func(t *testing.T, resp *http.Response, context string) {
		t.Helper()
		for _, header := range []string{
			"Access-Control-Allow-Origin", "Access-Control-Allow-Methods",
			"Access-Control-Allow-Headers", "Access-Control-Expose-Headers",
			"Access-Control-Allow-Credentials",
		} {
			if value := resp.Header.Get(header); value != "" {
				t.Errorf("%s: unexpected %s=%q", context, header, value)
			}
		}
	}

	// Allowlisted preflight is answered before bearer authentication.
	resp, body := request(srv, http.MethodOptions, "/mcp", geminiOrigin, "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("allowlisted preflight status = %d, want 204 (body %q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != geminiOrigin {
		t.Errorf("allowlisted preflight Access-Control-Allow-Origin = %q, want %q", got, geminiOrigin)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("allowlisted preflight Vary = %q, want it to contain Origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") || !strings.Contains(got, "OPTIONS") {
		t.Errorf("allowlisted preflight Allow-Methods = %q, want POST and OPTIONS", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Mcp-Session-Id") {
		t.Errorf("allowlisted preflight Allow-Headers = %q, want Mcp-Session-Id", got)
	}
	if got := resp.Header.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("allowlisted preflight Max-Age = %q, want 600", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allowlisted preflight must not allow credentials, got %q", got)
	}

	// The /mcp/stateless entry point and the root alias behave the same way.
	for _, path := range []string{"/mcp/stateless", "/"} {
		resp, body := request(srv, http.MethodOptions, path, geminiOrigin, "", "")
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("preflight %s status = %d, want 204 (body %q)", path, resp.StatusCode, body)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != geminiOrigin {
			t.Errorf("preflight %s Access-Control-Allow-Origin = %q, want %q", path, got, geminiOrigin)
		}
	}

	// A non-allowlisted origin is rejected without any CORS grant.
	resp, body = request(srv, http.MethodOptions, "/mcp", "https://evil.example", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied preflight status = %d, want 403 (body %q)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "origin denied") {
		t.Errorf("denied preflight body = %q, want origin denied", body)
	}
	noAccessControl(t, resp, "denied preflight")

	// A preflight without an Origin header is not a browser CORS request.
	resp, body = request(srv, http.MethodOptions, "/mcp", "", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("originless preflight status = %d, want 403 (body %q)", resp.StatusCode, body)
	}
	noAccessControl(t, resp, "originless preflight")

	// A real MCP response from an allowlisted origin exposes session headers.
	resp, body = request(srv, http.MethodPost, "/mcp/stateless", geminiOrigin, token, toolsListBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted tools/list status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != geminiOrigin {
		t.Errorf("allowlisted response Access-Control-Allow-Origin = %q, want %q", got, geminiOrigin)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("allowlisted response Vary = %q, want it to contain Origin", got)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Mcp-Session-Id") {
		t.Errorf("allowlisted response Expose-Headers = %q, want Mcp-Session-Id", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allowlisted response must not allow credentials, got %q", got)
	}

	// Non-allowlisted origins keep the existing guard behavior.
	resp, body = request(srv, http.MethodPost, "/mcp/stateless", "https://evil.example", token, toolsListBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("denied origin request status = %d, want 403 (body %q)", resp.StatusCode, body)
	}
	noAccessControl(t, resp, "denied origin request")

	// Requests without a token stay unauthorized and still advertise the challenge.
	resp, body = request(srv, http.MethodPost, "/mcp/stateless", geminiOrigin, "", toolsListBody)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("tokenless request status = %d, want 401 (body %q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got == "" {
		t.Errorf("tokenless request lost the WWW-Authenticate challenge")
	}

	// Unrelated endpoints are not touched by the compatibility layer.
	resp, _ = request(srv, http.MethodOptions, "/healthz", geminiOrigin, "", "")
	noAccessControl(t, resp, "healthz preflight")

	// With an empty allowlist the previous behavior is unchanged.
	_, bare, bareToken := geminiCompatTestApp(t, "", nil)
	resp, _ = request(bare, http.MethodOptions, "/mcp", geminiOrigin, "", "")
	noAccessControl(t, resp, "empty allowlist preflight")
	resp, body = request(bare, http.MethodPost, "/mcp/stateless", "", bareToken, toolsListBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty allowlist tools/list status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
	noAccessControl(t, resp, "empty allowlist response")
}
