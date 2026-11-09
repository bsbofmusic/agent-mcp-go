package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type httpScopeMarker struct{}

func TestGoSDKRetainsHTTPContextInToolCalls(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "transport-context-check", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "inspect_context"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]string, error) {
		mode, _ := ctx.Value(httpScopeMarker{}).(string)
		sid := ""
		if req != nil && req.Session != nil {
			sid = req.Session.ID()
		}
		return nil, map[string]string{"mode": mode, "session": sid}, nil
	})
	for _, stateless := range []bool{false, true} {
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
			&mcp.StreamableHTTPOptions{Stateless: stateless, JSONResponse: true, DisableLocalhostProtection: true})
		httpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), httpScopeMarker{}, "provided")
			handler.ServeHTTP(w, r.WithContext(ctx))
		})
		httpServer := httptest.NewServer(httpHandler)
		client := mcp.NewClient(&mcp.Implementation{Name: "identity-check-client", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint: httpServer.URL, HTTPClient: httpServer.Client(), DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			httpServer.Close()
			t.Fatal(err)
		}
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "inspect_context", Arguments: map[string]any{}})
		if err != nil || !strings.Contains(toolText(result), "provided") {
			t.Errorf("HTTP request context not propagated through official SDK stateless=%t: %v %s", stateless, err, toolText(result))
		}
		session.Close()
		httpServer.Close()
	}
}
