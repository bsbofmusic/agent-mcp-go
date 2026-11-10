package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func discoveryFixtureApp(t *testing.T, url string) *App {
	t.Helper()
	t.Setenv("AMG_DISCOVERY_FIXTURE_URL", url)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "route-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\"'\"'")+"' -test.run='^TestMCPDiscoveryHelperProcess$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return &App{config: &ConfigStore{cfg: Config{SharedMCPBin: wrapper}}}
}

func TestMCPDiscoveryHelperProcess(t *testing.T) {
	url := os.Getenv("AMG_DISCOVERY_FIXTURE_URL")
	if url == "" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "discovery-helper", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_routes"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"routes": []map[string]any{{"name": "fixture", "path": "/mcp/fixture", "url": url}}}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_tools"}, func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		if in["tool_name"] == "exact_tool" || in["tool_name"] == "wrong_requested" {
			return nil, map[string]any{"tool": map[string]any{"name": "exact_tool", "inputSchema": map[string]any{"type": "object", "description": "EXACT_FALLBACK_MARKER"}}}, nil
		}
		return nil, map[string]any{"tools": []map[string]any{{"name": "exact_tool", "description": "legacy summary"}}, "compact": true}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}
