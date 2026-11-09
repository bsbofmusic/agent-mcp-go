package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveNativeResourceExport(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live integration only")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hc := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 25 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "resource-live-audit", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	path := filepath.Join(cfg.StateDir, "resource-live-proof.txt")
	if e = os.WriteFile(path, []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	defer os.Remove(path)
	res, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "export_host_file", Arguments: map[string]any{"path": path}})
	if e != nil || res.IsError {
		t.Fatalf("export failed: %v", e)
	}
	uri := ""
	for _, content := range res.Content {
		if link, ok := content.(*mcp.ResourceLink); ok {
			uri = link.URI
		}
	}
	if uri == "" {
		t.Fatal("missing resource_link")
	}
	if e = os.WriteFile(path, []byte("modified"), 0600); e != nil {
		t.Fatal(e)
	}
	rd, e := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if e != nil {
		t.Fatal(e)
	}
	if len(rd.Contents) != 1 || string(rd.Contents[0].Blob) != "original" {
		t.Fatal("non-immutable resource contents")
	}
}
