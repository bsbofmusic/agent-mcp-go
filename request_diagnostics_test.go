package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type diagnosticBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *diagnosticBuffer) text() string { b.mu.Lock(); defer b.mu.Unlock(); return b.Buffer.String() }
func diagnosticEntries(t *testing.T, buffer *diagnosticBuffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.text()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestRequestDiagnosticsCancelBeforeHandlerReturns(t *testing.T) {
	var buffer diagnosticBuffer
	app := &App{requestLogger: log.New(&buffer, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	handler := app.requestDiagnosticsMiddleware(requestDispatchMiddleware(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		logRequestPhase(ctx, "review_lock")
		close(entered)
		<-release
		return nil, ctx.Err()
	}))
	go func() {
		defer close(returned)
		_, _ = handler(ctx, "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "exec_command", Arguments: json.RawMessage(`{"cmd":"PRIVATE_COMMAND","scope":"PRIVATE_SCOPE"}`)}})
	}()
	<-entered
	cancel()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	observed := false
	for !observed {
		select {
		case <-deadline.C:
			close(release)
			<-returned
			t.Fatal("cancellation was not logged while handler remained blocked")
		case <-ticker.C:
			observed = strings.Contains(buffer.text(), `"phase":"context_done"`)
		}
	}
	close(release)
	<-returned
	entries := diagnosticEntries(t, &buffer)
	if len(entries) != 5 {
		t.Fatalf("want receive/dispatch/lock/cancel/finish: %s", buffer.text())
	}
	id := entries[0]["request_id"]
	for _, entry := range entries {
		if entry["request_id"] != id || entry["tool"] != "exec_command" {
			t.Fatalf("correlation lost: %v", entry)
		}
	}
	if entries[4]["outcome"] != "canceled" {
		t.Fatalf("wrong outcome: %v", entries[4])
	}
	if strings.Contains(buffer.text(), "PRIVATE_") {
		t.Fatal("arguments leaked")
	}
}

func TestRequestDiagnosticsSafeErrorsAndUnknownNames(t *testing.T) {
	cases := []struct {
		label  string
		result mcp.Result
		err    error
		want   string
	}{
		{"error", nil, errors.New("PRIVATE_ERROR_URL_TOKEN"), "error"},
		{"tool error", &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "PRIVATE_OUTPUT"}}}, nil, "tool_error"},
		{"deadline", nil, context.DeadlineExceeded, "deadline"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var buffer diagnosticBuffer
			app := &App{requestLogger: log.New(&buffer, "", 0)}
			handler := app.requestDiagnosticsMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { return tc.result, tc.err })
			_, _ = handler(context.Background(), "tools/call", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "PRIVATE_UNKNOWN_TOOL"}})
			_, _ = handler(context.Background(), "PRIVATE_UNKNOWN_METHOD", nil)
			if strings.Contains(buffer.text(), "PRIVATE_") {
				t.Fatal("untrusted names/errors/results leaked")
			}
			entries := diagnosticEntries(t, &buffer)
			if entries[1]["tool"] != "other" || entries[1]["outcome"] != tc.want || entries[2]["method"] != "other" {
				t.Fatalf("bad safe diagnostic labels: %s", buffer.text())
			}
		})
	}
}

func TestRequestDiagnosticsPanicDoesNotLogPanicPayload(t *testing.T) {
	var buffer diagnosticBuffer
	app := &App{requestLogger: log.New(&buffer, "", 0)}
	handler := app.requestDiagnosticsMiddleware(func(context.Context, string, mcp.Request) (mcp.Result, error) { panic("PRIVATE_PANIC") })
	func() {
		defer func() {
			if recover() != "PRIVATE_PANIC" {
				t.Error("panic propagation changed")
			}
		}()
		_, _ = handler(context.Background(), "ping", nil)
	}()
	if strings.Contains(buffer.text(), "PRIVATE_") || !strings.Contains(buffer.text(), `"outcome":"panic"`) {
		t.Fatalf("unsafe panic diagnostics: %s", buffer.text())
	}
}

func TestRequestDiagnosticsSingleSafeLoggerBoundary(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "request_diagnostics.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := selector.X.(*ast.SelectorExpr)
		if !ok || owner.Sel.Name != "logger" {
			return true
		}
		calls++
		convert, valid := func() (*ast.CallExpr, bool) {
			if len(call.Args) != 1 {
				return nil, false
			}
			x, ok := call.Args[0].(*ast.CallExpr)
			return x, ok
		}()
		if !valid || selector.Sel.Name != "Print" || len(convert.Args) != 1 {
			t.Error("diagnostics must use the single encoded event boundary")
			return true
		}
		function, ok := convert.Fun.(*ast.Ident)
		value, okValue := convert.Args[0].(*ast.Ident)
		if !ok || !okValue || function.Name != "string" || value.Name != "encoded" {
			t.Error("raw diagnostic value bypasses event encoding")
		}
		return true
	})
	if calls != 1 {
		t.Fatalf("want one safe logger exit, got %d", calls)
	}
}

func TestRequestDiagnosticsHTTPBothModesPreserveProtocolAndPrivacy(t *testing.T) {
	for _, endpoint := range []string{"/mcp", "/mcp/stateless"} {
		t.Run(endpoint, func(t *testing.T) {
			app, srv, token := testApp(t)
			var buffer diagnosticBuffer
			app.requestLogger = log.New(&buffer, "", 0)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "PRIVATE_CLIENT_NAME", Version: "test"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + endpoint, HTTPClient: &http.Client{Transport: testAuthenticatedHostTransport{token: token}}, DisableStandaloneSSE: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{"path": "PRIVATE_FILENAME", "content": "PRIVATE_FILE_CONTENT"}})
			if err != nil || out.IsError {
				t.Fatalf("write failed: %v %v", out, err)
			}
			if _, err = session.ListTools(ctx, nil); err != nil {
				t.Fatal(err)
			}
			// Let transport teardown finish before inspecting logs.
			if err = session.Close(); err != nil {
				t.Fatal(err)
			}
			text := buffer.text()
			if strings.Contains(text, "PRIVATE_") || strings.Contains(text, token) {
				t.Fatal("request details or bearer leaked")
			}
			entries := diagnosticEntries(t, &buffer)
			phases := map[string]bool{}
			httpIDs := map[any]bool{}
			for _, entry := range entries {
				if strings.HasPrefix(entry["request_id"].(string), "http-") {
					httpIDs[entry["request_id"]] = true
				}
			}
			foundParent := false
			for _, entry := range entries {
				if entry["tool"] == "write_file" {
					phases[entry["phase"].(string)] = true
					if httpIDs[entry["parent_id"]] {
						foundParent = true
					}
				}
			}
			for _, phase := range []string{"received", "review_locate", "review_lock", "review_baseline", "handler_start", "finished"} {
				if !phases[phase] {
					t.Fatalf("missing %s: %s", phase, text)
				}
			}
			if !foundParent {
				t.Fatalf("HTTP/RPC correlation lost: %s", text)
			}
		})
	}
}

func TestRequestDiagnosticsSlowWarningIsBoundedAndPhaseAware(t *testing.T) {
	var buffer diagnosticBuffer
	app := &App{requestLogger: log.New(&buffer, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, finish := app.traceRequest(ctx, "rpc", "tools/call", "read_file")
	_, finishHTTP := app.traceRequest(context.Background(), "http", "stateless", "")
	_, finishIdle := app.traceRequest(context.Background(), "rpc", "other", "")
	logRequestPhase(ctx, "handler_start")
	logRequestPhase(ctx, "review_lock")
	deadline := time.NewTimer(2500 * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !strings.Contains(buffer.text(), `"phase":"slow"`) {
		select {
		case <-deadline.C:
			finish("ok")
			t.Fatal("no in-flight slow warning before handler returned")
		case <-ticker.C:
		}
	}
	ctx.Value(requestTraceKey{}).(*requestTrace).warnSlow()
	finishHTTP("returned")
	finishIdle("ok")
	finish("ok")
	ctx.Value(requestTraceKey{}).(*requestTrace).warnSlow()
	cancel()
	entries := diagnosticEntries(t, &buffer)
	count := 0
	for _, e := range entries {
		if e["phase"] == "slow" {
			count++
			if e["method"] != "tools/call" || e["level"] != "WARN" || e["stage"] != "review_lock" || e["elapsed_ms"].(float64) < 2000 {
				t.Fatalf("missing slow phase evidence: %v", e)
			}
		}
	}
	if count != 1 || entries[len(entries)-1]["phase"] != "finished" {
		t.Fatalf("want one warning then finish: %s", buffer.text())
	}
	// The warning must neither cancel nor replace the operation's outcome.
	if entries[len(entries)-1]["outcome"] != "ok" {
		t.Fatal("warning changed outcome")
	}
}

func TestRequestDiagnosticsFinishedCannotEmitSlowOrLatePhase(t *testing.T) {
	var buffer diagnosticBuffer
	app := &App{requestLogger: log.New(&buffer, "", 0)}
	ctx, finish := app.traceRequest(context.Background(), "rpc", "tools/call", "read_file")
	finish("ok")
	ctx.Value(requestTraceKey{}).(*requestTrace).warnSlow()
	logRequestPhase(ctx, "file_read")
	if entries := diagnosticEntries(t, &buffer); len(entries) != 2 || entries[1]["phase"] != "finished" {
		t.Fatalf("late diagnostics: %s", buffer.text())
	}
}
