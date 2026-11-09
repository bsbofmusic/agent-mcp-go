package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveIsolationAndConcurrentClients(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live only")
	}
	c, e := liveTestConfig(t)
	if e != nil {
		t.Fatal(e)
	}
	token, e := readBearerToken(c.TokenFile)
	if e != nil {
		t.Fatal(e)
	}
	endpoint := "http://" + c.Listen + "/mcp"
	if v := os.Getenv("AGENT_MCP_GO_ENDPOINT"); v != "" {
		endpoint = v
	}
	const clients = 6
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			hc := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 30 * time.Second}
			client := mcp.NewClient(&mcp.Implementation{Name: fmt.Sprintf("pressure-%d", id), Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
			if err != nil {
				errs <- fmt.Errorf("connect %d: %w", id, err)
				return
			}
			defer session.Close()
			if _, err := session.ListTools(ctx, nil); err != nil {
				errs <- fmt.Errorf("list %d: %w", id, err)
				return
			}
			if id == 0 {
				bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{
					"source": "route-that-must-not-exist", "tool": "search_memory",
				}})
				if err == nil && !bad.IsError {
					errs <- fmt.Errorf("invalid MCP route unexpectedly accepted")
					return
				}
			}
			good, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_environment", Arguments: map[string]any{}})
			if err != nil || good.IsError {
				errs <- fmt.Errorf("healthy tool failed after broken route %d: %v", id, err)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
