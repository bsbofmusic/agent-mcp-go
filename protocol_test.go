package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	token      string
	underlying http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.Clone(r.Context())
	q.Header = r.Header.Clone()
	q.Header.Set("Authorization", "Bearer "+b.token)
	if os.Getenv("AGENT_MCP_GO_TRACE") == "1" {
		fmt.Printf("CLIENT_HTTP origin=%q host=%q method=%q\n", r.Header.Get("Origin"), r.Host, r.Method)
	}
	resp, err := b.underlying.RoundTrip(q)
	if os.Getenv("AGENT_MCP_GO_TRACE") == "1" && resp != nil && resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		fmt.Printf("CLIENT_HTTP_FAIL status=%d body_prefix=%q\n", resp.StatusCode, string(body))
		resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), resp.Body))
	}
	return resp, err
}

func TestLiveMCPProtocol(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("set AGENT_MCP_GO_LIVE=1 for production loopback smoke")
	}
	config, err := liveTestConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readBearerToken(config.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	transport := bearerTransport{token: string(token), underlying: http.DefaultTransport}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-mcp-go-live-smoke", Version: "1"}, nil)
	endpoint := "http://" + config.Listen + "/mcp"
	if external := os.Getenv("AGENT_MCP_GO_ENDPOINT"); external != "" {
		endpoint = external
	}
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	listing, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"exec_command": false, "skills_list": false, "shared_mcp_routes": false, "transfer_prepare": false}
	for _, tool := range listing.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("missing MCP tool %s", n)
		}
	}
	out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_environment", Arguments: map[string]any{}})
	if err != nil || out.IsError {
		t.Fatalf("environment error %v", err)
	}
	if config.SkillsRoot != "" {
		skill, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "skills_list", Arguments: map[string]any{}})
		if err != nil || skill.IsError {
			t.Fatalf("skills_list error %v", err)
		}
	}
	if config.SharedMCPBin != "" {
		routes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp_list_sources", Arguments: map[string]any{}})
		if err != nil || routes.IsError {
			t.Fatalf("shared source discovery failure %v", err)
		}
	}
	cmd, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"cmd": "printf agentmcpgo-ok", "workdir": config.WorkRoot, "yield_time_ms": 10000}})
	if err != nil || cmd.IsError {
		t.Fatalf("command failure %v %s", err, toolText(cmd))
	}
	if !strings.Contains(toolText(cmd), "agentmcpgo-ok") {
		t.Errorf("command output missing: %s", toolText(cmd))
	}
}

func TestLiveFullCatalogAndSharedMemory(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_MEMORY_TEST") != "1" {
		t.Skip("set AGENT_MCP_GO_MEMORY_TEST=1 for the optional MemOS integration")
	}
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live test only")
	}
	cfg, e := liveTestConfig(t)
	if e != nil {
		t.Fatal(e)
	}
	token, e := readBearerToken(cfg.TokenFile)
	if e != nil {
		t.Fatal(e)
	}
	endpoint := "http://" + cfg.Listen + "/mcp"
	if ext := os.Getenv("AGENT_MCP_GO_ENDPOINT"); ext != "" {
		endpoint = ext
	}
	cl := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 60 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	session, e := mcp.NewClient(&mcp.Implementation{Name: "parity-audit", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: cl, DisableStandaloneSSE: true,
	}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	listed, e := session.ListTools(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	found := map[string]bool{}
	for _, v := range listed.Tools {
		found[v.Name] = true
	}
	original := []string{
		"read_file", "write_file", "import_host_file", "export_host_file", "run_command",
		"git_status", "show_changes", "git_push", "git_commit", "git_log", "glob", "grep",
		"list_directory", "tree", "apply_patch", "exec_command", "write_stdin", "view_image",
		"update_plan", "clock_curr_time", "clock_sleep", "get_environment", "get_project_doc",
		"get_agent_brief", "remember", "recall", "skills_list", "skills_read",
		"mcp_list_sources", "mcp_search_tools", "mcp_get_tool", "mcp_call_tool",
	}
	for _, name := range original {
		if !found[name] {
			t.Errorf("missing original tool: %s", name)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	invoke := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, er := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if er != nil || res.IsError {
			t.Fatalf("%s failed: %v %s", name, er, toolText(res))
		}
		return res
	}
	sources := invoke("mcp_list_sources", map[string]any{})
	if !strings.Contains(toolText(sources), "memos") {
		t.Fatalf("dynamic MemOS route not discovered")
	}
	match := invoke("mcp_search_tools", map[string]any{"source": "memos", "query": "search_memory", "limit": 5})
	if !strings.Contains(toolText(match), "search_memory") {
		t.Fatalf("MemOS search_memory tool not searchable")
	}
	schema := invoke("mcp_get_tool", map[string]any{"source": "memos", "tool": "search_memory"})
	if !strings.Contains(toolText(schema), "search_memory") {
		t.Fatalf("MemOS tool schema lookup failed")
	}
	// Verify that the full generic dispatch path reaches the real MemOS
	// owner. Inspect success only; do not print or archive private memories.
	_ = invoke("mcp_call_tool", map[string]any{
		"source": "memos",
		"tool":   "search_memory",
		"arguments": map[string]any{
			"query":                      "agent-mcp-go shared tool integration smoke test",
			"conversation_first_message": "agent mcp go integration test",
			"memory_limit_number":        1,
		},
	})
}

func TestLiveUnscopedToolSearch(t *testing.T) {
	query := os.Getenv("AGENT_MCP_GO_SEARCH_TEST_TOOL")
	if query == "" {
		t.Skip("set AGENT_MCP_GO_SEARCH_TEST_TOOL to a tool in your shared catalog")
	}
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live only")
	}
	cfg, e := liveTestConfig(t)
	if e != nil {
		t.Fatal(e)
	}
	token, e := readBearerToken(cfg.TokenFile)
	if e != nil {
		t.Fatal(e)
	}
	endpoint := "http://" + cfg.Listen + "/mcp"
	if v := os.Getenv("AGENT_MCP_GO_ENDPOINT"); v != "" {
		endpoint = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	hc := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 80 * time.Second}
	session, e := mcp.NewClient(&mcp.Implementation{Name: "search-audit", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp_search_tools", Arguments: map[string]any{
		"query": query, "limit": 8,
	}})
	if e != nil || result.IsError {
		t.Fatalf("unscoped search failed: %v", e)
	}
	if !strings.Contains(toolText(result), query) {
		t.Fatal("search did not discover tool in an unrelated route name")
	}
	warmStart := time.Now()
	again, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp_search_tools", Arguments: map[string]any{
		"query": query, "limit": 8,
	}})
	if err != nil || again.IsError {
		t.Fatalf("warm tool lookup failed: %v", err)
	}
	if took := time.Since(warmStart); took > 4*time.Second {
		t.Fatalf("ephemeral catalog cache failed to accelerate repeat lookup: %s", took)
	}
}
func toolText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, v := range res.Content {
		if x, ok := v.(*mcp.TextContent); ok {
			b.WriteString(x.Text)
		}
	}
	return b.String()
}
