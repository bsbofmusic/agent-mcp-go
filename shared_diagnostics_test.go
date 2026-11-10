package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedDiagnosticsBlockedNativeStages(t *testing.T) {
	for _, stage := range []string{"upstream_connect", "upstream_call", "upstream_close"} {
		t.Run(stage, func(t *testing.T) {
			reached, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			gate := func(ctx context.Context) {
				close(reached)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			var lists atomic.Int32
			upstream := mcp.NewServer(&mcp.Implementation{Name: "PRIVATE_UPSTREAM", Version: "1"}, nil)
			upstream.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "tools/list" {
						lists.Add(1)
					}
					if stage == "upstream_connect" && method == "initialize" {
						gate(ctx)
					}
					return next(ctx, method, req)
				}
			})
			upstream.AddTool(&mcp.Tool{Name: "PRIVATE_TOOL", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if stage == "upstream_call" {
					gate(ctx)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE_RESULT"}}, Meta: mcp.Meta{"PRIVATE_META": "preserved"}, IsError: true}, nil
			})
			transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{JSONResponse: true})
			web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stage == "upstream_close" && r.Method == http.MethodDelete {
					gate(r.Context())
				}
				transport.ServeHTTP(w, r)
			}))
			defer web.Close()
			// Unblock before httptest shutdown even if an assertion fails.
			defer unblock()
			f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "PRIVATE_SOURCE", "path": "/mcp/PRIVATE_SOURCE", "url": web.URL}}})
			var buffer diagnosticBuffer
			f.app.requestLogger = log.New(&buffer, "", 0)
			traced := make(chan *requestTrace, 1)
			outer := mcp.NewServer(&mcp.Implementation{Name: "trace-outer", Version: "1"}, nil)
			f.app.registerMCPParity(outer)
			outer.AddReceivingMiddleware(f.app.requestDiagnosticsMiddleware, func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "tools/call" {
						traced <- ctx.Value(requestTraceKey{}).(*requestTrace)
					}
					return next(ctx, method, req)
				}
			})
			client := contractClient(t, outer)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				value *mcp.CallToolResult
				err   error
			}
			done := make(chan result, 1)
			go func() {
				v, e := client.CallTool(ctx, &mcp.CallToolParams{Name: "mcp_call_tool", Arguments: map[string]any{"source": "PRIVATE_SOURCE", "tool": "PRIVATE_TOOL", "arguments": map[string]any{"secret": "PRIVATE_ARGUMENT"}}})
				done <- result{v, e}
			}()
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("upstream did not reach gate")
			}
			trace := <-traced
			// Exercise the real blocked call's current stage without waiting two seconds
			// in each fixture; the timer itself has its own boundedness regression.
			trace.warnSlow()
			entries := diagnosticEntries(t, &buffer)
			slow := entries[len(entries)-1]
			if slow["phase"] != "slow" || slow["stage"] != stage {
				t.Fatalf("wrong blocked stage: %v", slow)
			}
			unblock()
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("call did not finish after release")
			}
			if got.err != nil || got.value == nil || !got.value.IsError || got.value.Meta["PRIVATE_META"] != "preserved" {
				t.Fatalf("native result changed: %+v %v", got.value, got.err)
			}
			trace.warnSlow()
			slowCount := 0
			var phases []string
			for _, entry := range diagnosticEntries(t, &buffer) {
				if entry["request_id"] != trace.id {
					continue
				}
				phase := entry["phase"].(string)
				if phase == "slow" {
					slowCount++
					continue
				}
				phases = append(phases, phase)
			}
			want := []string{"received", "route_discovery", "helper_connect", "helper_call", "helper_close", "upstream_connect", "upstream_call", "upstream_close", "finished"}
			if !reflect.DeepEqual(phases, want) || slowCount != 1 || lists.Load() != 0 {
				t.Fatalf("call lifecycle changed: phases=%v slow=%d lists=%d", phases, slowCount, lists.Load())
			}
			if strings.Contains(buffer.text(), "PRIVATE_") || strings.Contains(buffer.text(), web.URL) {
				t.Fatal("dynamic target/arguments/results leaked into diagnostics")
			}
		})
	}
}

func TestSharedDiagnosticsFallbackWorkersDoNotOverwriteScan(t *testing.T) {
	const endpoint = "http://PRIVATE_ENDPOINT.invalid"
	f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "PRIVATE_SOURCE", "path": "/mcp/PRIVATE_SOURCE", "url": endpoint}}, Tools: map[string][]map[string]any{endpoint: {{"name": "echo", "description": "fixture"}}}})
	var buffer diagnosticBuffer
	f.app.requestLogger = log.New(&buffer, "", 0)
	ctx, finish := f.app.traceRequest(context.Background(), "rpc", "tools/call", "mcp_search_tools")
	out, err := f.app.searchRouteTools(ctx, SearchMCPInput{Query: "echo"})
	finish(diagnosticOutcome(ctx, nil, err))
	if err != nil || len(out.Matches) != 1 {
		t.Fatalf("fallback changed: %+v %v", out, err)
	}
	entries := diagnosticEntries(t, &buffer)
	seenScan := false
	for _, entry := range entries {
		if entry["phase"] == "module_scan" {
			seenScan = true
			continue
		}
		if seenScan && entry["phase"] != "finished" {
			t.Fatalf("parallel worker overwrote parent stage: %v", entry)
		}
	}
	if !seenScan || entries[len(entries)-1]["stage"] != "module_scan" {
		t.Fatal("scan stage missing")
	}
	if strings.Contains(buffer.text(), "PRIVATE_") {
		t.Fatal("fallback metadata leaked")
	}
}
