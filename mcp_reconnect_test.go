package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reconnectRequests struct {
	initializes, lists, writes atomic.Int32
}

func (seen *reconnectRequests) record(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var rpc struct {
		Method string
		Params struct{ Name string }
	}
	_ = json.Unmarshal(body, &rpc)
	switch rpc.Method {
	case "initialize":
		seen.initializes.Add(1)
	case "tools/list":
		seen.lists.Add(1)
	case "tools/call":
		if rpc.Params.Name == "write_file" {
			seen.writes.Add(1)
			return true
		}
	}
	return false
}

func reconnectClient(t *testing.T, ctx context.Context, app *App, web *httptest.Server, token, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "local-reconnect-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: web.URL + endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: handoverHTTPTransport{
			base: web.Client().Transport, host: app.config.Get().Listen, token: token,
		}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2025-11-25"})
	if err != nil {
		t.Fatalf("reinitialize %s: %v", endpoint, err)
	}
	t.Cleanup(func() { session.Close() })
	if (endpoint == "/mcp") != (session.ID() != "") {
		t.Fatalf("unexpected session identity for %s", endpoint)
	}
	return session
}

func reconnectToolNames(t *testing.T, ctx context.Context, session *mcp.ClientSession) map[string]bool {
	t.Helper()
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list after initialize: %v", err)
	}
	names := make(map[string]bool)
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}

// A lost response does not tell the caller whether the write committed. Recovery
// explicitly creates a fresh session and reads state; it must not replay the write.
func TestMCPReconnectAfterLostWriteResponse(t *testing.T) {
	for _, endpoint := range []string{"/mcp", "/mcp/stateless"} {
		for _, failure := range []string{"cancel", "disconnect"} {
			t.Run(strings.TrimPrefix(endpoint, "/")+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				app, original, token := testApp(t)
				original.Close()
				var seen reconnectRequests
				var faulted atomic.Bool
				committed, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				handler := app.handler()
				web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !seen.record(r) || !faulted.CompareAndSwap(false, true) {
						handler.ServeHTTP(w, r)
						return
					}
					buffer := httptest.NewRecorder()
					handler.ServeHTTP(buffer, r)
					close(committed)
					if failure == "cancel" {
						select {
						case <-release:
						case <-r.Context().Done():
						}
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("inject disconnect: %v", err)
						return
					}
					conn.Close()
				}))
				t.Cleanup(web.Close)
				t.Cleanup(unblock)
				first := reconnectClient(t, ctx, app, web, token, endpoint)
				before := reconnectToolNames(t, ctx, first)
				if !before["write_file"] || !before["get_agent_brief"] {
					t.Fatal("fixture did not expose the real application tools")
				}
				const scope, note, step = "RECONNECT_TASK", "RECONNECT_NOTE", "RECONNECT_PLAN"
				handoverOK(t, ctx, first, "remember", map[string]any{"scope": scope, "key": "checkpoint", "value": note}, "")
				handoverOK(t, ctx, first, "update_plan", map[string]any{"scope": scope, "plan": []map[string]string{{"step": step, "status": "in_progress"}}}, "")
				callCtx, stopCall := context.WithCancel(ctx)
				defer stopCall()
				done := make(chan error, 1)
				go func() {
					_, err := first.CallTool(callCtx, &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{"path": "reconnect-write.txt", "content": "COMMITTED_ONCE"}})
					done <- err
				}()
				select {
				case <-committed:
				case <-ctx.Done():
					t.Fatal("write did not reach the injected failure")
				}
				if failure == "cancel" {
					stopCall()
				}
				select {
				case err := <-done:
					if err == nil || (failure == "cancel" && !errors.Is(err, context.Canceled)) {
						t.Fatalf("lost response must be an error (%s): %v", failure, err)
					}
				case <-ctx.Done():
					t.Fatal("failed write call did not return")
				}
				unblock()
				first.Close()
				fresh := reconnectClient(t, ctx, app, web, token, endpoint)
				if endpoint == "/mcp" && fresh.ID() == first.ID() {
					t.Fatal("recovery reused the terminated MCP session")
				}
				if after := reconnectToolNames(t, ctx, fresh); !reflect.DeepEqual(before, after) {
					t.Fatal("reinitialized application catalogue was incomplete")
				}
				brief := handoverOK(t, ctx, fresh, "get_agent_brief", map[string]any{"scope": scope}, "")
				notes := handoverOK(t, ctx, fresh, "recall", map[string]any{"scope": scope}, "")
				if !containsAll(brief, note, step) || !strings.Contains(notes, note) {
					t.Fatal("new session did not restore the explicit scope plan and notes")
				}
				unknown := handoverOK(t, ctx, fresh, "get_agent_brief", map[string]any{"scope": "RECONNECT_UNKNOWN"}, "")
				if !strings.Contains(unknown, "No saved plan or notes exist") || strings.Contains(unknown, note) || strings.Contains(unknown, step) {
					t.Fatal("unknown scope consumed another task")
				}
				raw, err := os.ReadFile(filepath.Join(app.config.Get().WorkRoot, "reconnect-write.txt"))
				if err != nil || string(raw) != "COMMITTED_ONCE" {
					t.Fatalf("lost-response write did not commit: %q %v", raw, err)
				}
				if seen.writes.Load() != 1 || seen.initializes.Load() != 2 || seen.lists.Load() != 2 {
					t.Fatalf("unexpected replay/recovery requests: writes=%d initialize=%d list=%d", seen.writes.Load(), seen.initializes.Load(), seen.lists.Load())
				}
			})
		}
	}
}

func TestMCPReconnectRefreshesDynamicToolList(t *testing.T) {
	for _, endpoint := range []string{"/mcp", "/mcp/stateless"} {
		t.Run(strings.TrimPrefix(endpoint, "/"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			app, original, token := testApp(t)
			original.Close()
			server := mcp.NewServer(&mcp.Implementation{Name: "dynamic-reconnect-fixture", Version: "1"}, nil)
			add := func(name string) {
				mcp.AddTool(server, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, TextOut, error) {
					return nil, TextOut{"fixture"}, nil
				})
			}
			add("before_reconnect")
			handler := app.auth(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
				Stateless: endpoint == "/mcp/stateless", JSONResponse: true, DisableLocalhostProtection: true,
			}))
			var seen reconnectRequests
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.record(r)
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(web.Close)
			first := reconnectClient(t, ctx, app, web, token, endpoint)
			if got := reconnectToolNames(t, ctx, first); len(got) != 1 || !got["before_reconnect"] {
				t.Fatal("initial dynamic catalogue mismatch")
			}
			first.Close()
			server.RemoveTools("before_reconnect")
			add("after_reconnect")
			fresh := reconnectClient(t, ctx, app, web, token, endpoint)
			if got := reconnectToolNames(t, ctx, fresh); len(got) != 1 || !got["after_reconnect"] || got["before_reconnect"] {
				t.Fatal("new session retained the obsolete tool list")
			}
			if seen.initializes.Load() != 2 || seen.lists.Load() != 2 {
				t.Fatal("catalogue refresh skipped initialize or tools/list")
			}
		})
	}
}
