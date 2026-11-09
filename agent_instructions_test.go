package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Regression for the mechanism in CodexFree v1.9.1:
// server.rs::get_info -> instructions.rs::build_initial_instructions.
// It must appear in the actual initialize response, not just a callable tool.
func TestCodexFreeStyleInitializationBrief(t *testing.T) {
	a, _, token := testApp(t)
	cfg := a.config.Get()
	skillDir := filepath.Join(cfg.SkillsRoot, "owner-one")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: owner-one\ndescription: Operate the test owner's service safely\n---\n# Owner instructions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkRoot, "AGENTS.md"), []byte("PROJECT_MARKER_0234: Prefer reading the existing repository before edits."), 0600); err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"amg-instruction-regression","version":"1"}}}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3100/mcp", strings.NewReader(body))
	request.Host = "127.0.0.1:3100"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	instructions := got.Result.Instructions
	if instructions == "" {
		t.Fatal("initialize response lacks agent instructions")
	}
	ordered := []string{"You are operating a real", "## Environment", "## Available Skills", "owner-one", "## Project instructions", "PROJECT_MARKER_0234"}
	pos := -1
	for _, item := range ordered {
		next := strings.Index(instructions, item)
		if next <= pos {
			t.Fatalf("instructions misplaced or missing %q after index %d", item, pos)
		}
		pos = next
	}
	if !strings.Contains(instructions, "skills_read") || !strings.Contains(instructions, "mcp_search_tools") || !strings.Contains(instructions, "MemOS") {
		t.Fatalf("missing native tool-routing guidance")
	}
	if len(instructions) > maxBootstrapInstructionsBytes || !utf8.ValidString(instructions) {
		t.Fatalf("instructions exceeded bound or contain invalid UTF-8: %d bytes", len(instructions))
	}
}

func TestNativeAgentBriefRefreshesSkillCatalogAndBoundsLongDescriptions(t *testing.T) {
	a, _, _ := testApp(t)
	cfg := a.config.Get()
	if err := os.MkdirAll(cfg.SkillsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	first := a.buildAgentInstructions()
	if !strings.Contains(first, "## Available Skills") {
		t.Fatalf("missing skills section")
	}
	if !strings.Contains(first, "use skills_list") {
		t.Fatalf("missing fallback discovery guidance")
	}
	// No restart or second registry is required for get_agent_brief to notice
	// canonical Skills Manager additions.
	skill := filepath.Join(cfg.SkillsRoot, "dynamic-owner", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: dynamic-owner\ndescription: "+strings.Repeat("长", 300)+"\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second := a.buildAgentInstructions()
	if strings.Contains(first, "dynamic-owner") || !strings.Contains(second, "dynamic-owner") {
		t.Fatalf("catalogue failed to reflect on-disk Skill change")
	}
	if strings.Contains(second, strings.Repeat("长", 300)) {
		t.Fatalf("large Skill description was not bounded")
	}
	if !utf8.ValidString(second) || len(second) > maxBootstrapInstructionsBytes {
		t.Fatalf("invalid or oversized Skill guidance")
	}
	if strings.Contains(second, "SKILL.md body:") {
		t.Fatalf("skill bodies must remain on-demand")
	}
}

func TestNewConnectionsRefreshInitializationInstructions(t *testing.T) {
	a, _, token := testApp(t)
	handler := a.handler()
	cfg := a.config.Get()
	initialize := func(path string) string {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fresh-client","version":"1"}}}`
		discover := strings.HasSuffix(path, "#discover")
		path = strings.TrimSuffix(path, "#discover")
		if discover {
			body = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fresh-client","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
		}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3100"+path, strings.NewReader(body))
		if discover {
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "server/discover")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var response struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Result.Instructions == "" {
			t.Fatalf("initialize failed (%s): %d %s", path, rec.Code, rec.Body.String())
		}
		return response.Result.Instructions
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		if strings.Contains(initialize(path), "fresh-owner-marker") {
			t.Fatal("unexpected fixture marker")
		}
	}
	skill := filepath.Join(cfg.SkillsRoot, "fresh-owner-marker", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: fresh-owner-marker\ndescription: Fresh owner metadata\n---\nPRIVATE_BODY_NOT_BOOTSTRAP"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkRoot, "AGENTS.md"), []byte("FRESH_PROJECT_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		fresh := initialize(path)
		if !containsAll(fresh, "fresh-owner-marker", "FRESH_PROJECT_MARKER") {
			t.Errorf("new %s connection received stale instructions", path)
		}
		if strings.Contains(fresh, "PRIVATE_BODY_NOT_BOOTSTRAP") {
			t.Error("bootstrap eagerly loaded Skill body")
		}
	}
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		if strings.Contains(initialize(path), "fresh-owner-marker") {
			t.Errorf("removed owner remains in %s initialization", path)
		}
	}
}

func TestLiveBootstrap(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live endpoint smoke only")
	}
	c, err := liveTestConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readBearerToken(c.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + c.Listen + "/mcp"
	if remote := os.Getenv("AGENT_MCP_GO_ENDPOINT"); remote != "" {
		endpoint = remote
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "amg-bootstrapping-smoke", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	instructions := conn.InitializeResult().Instructions
	for _, required := range []string{"## Environment", "## Available Skills"} {
		if !strings.Contains(instructions, required) {
			t.Errorf("server initialize lacks %q", required)
		}
	}
	if len(instructions) > maxBootstrapInstructionsBytes || !utf8.ValidString(instructions) {
		t.Fatalf("server initialize instructions oversized or invalid UTF-8 (%d bytes)", len(instructions))
	}
	if strings.Contains(instructions, string(token)) {
		t.Fatal("server initialize leaked Bearer token")
	}
}
