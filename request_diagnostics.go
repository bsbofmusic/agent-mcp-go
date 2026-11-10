package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var diagnosticSequence atomic.Uint64

type requestTraceKey struct{}

type requestTrace struct {
	logger   *log.Logger
	id       string
	parent   string
	method   string
	tool     string
	started  time.Time
	mu       sync.Mutex
	finished bool
	warned   bool
	stage    string
}

// All labels come from server-owned constants, never arguments, headers, error
// text, request IDs supplied by clients, results, paths or task scope values.
func (t *requestTrace) emit(phase, outcome string) {
	entry := struct {
		Event   string `json:"event"`
		ID      string `json:"request_id"`
		Parent  string `json:"parent_id,omitempty"`
		Method  string `json:"method"`
		Tool    string `json:"tool,omitempty"`
		Phase   string `json:"phase"`
		Outcome string `json:"outcome,omitempty"`
		Elapsed int64  `json:"elapsed_ms"`
		Level   string `json:"level,omitempty"`
		Stage   string `json:"stage,omitempty"`
	}{"amg_request", t.id, t.parent, t.method, t.tool, phase, outcome, time.Since(t.started).Milliseconds(), "", ""}
	if phase == "slow" {
		entry.Level = "WARN"
	}
	if phase == "slow" || phase == "finished" || phase == "context_done" {
		entry.Stage = t.stage
	}
	encoded, _ := json.Marshal(entry)
	t.logger.Print(string(encoded))
}

// warnSlow reports once per active RPC. It does not cancel, retry or change it.
func (t *requestTrace) warnSlow() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || t.warned {
		return
	}
	t.warned = true
	t.emit("slow", "")
}

func (a *App) traceRequest(ctx context.Context, prefix, method, tool string) (context.Context, func(string)) {
	if a.requestLogger == nil {
		return ctx, func(string) {}
	}
	parent := ""
	if trace, ok := ctx.Value(requestTraceKey{}).(*requestTrace); ok {
		parent = trace.id
	}
	trace := &requestTrace{logger: a.requestLogger, id: fmt.Sprintf("%s-%d", prefix, diagnosticSequence.Add(1)), parent: parent, method: method, tool: tool, started: time.Now(), stage: "received"}
	trace.emit("received", "")
	// Long-lived HTTP notification/SSE traffic is not a slow tool operation.
	var slow *time.Timer
	if prefix == "rpc" && method != "other" {
		slow = time.AfterFunc(2*time.Second, trace.warnSlow)
	}
	stop := context.AfterFunc(ctx, func() {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		if !trace.finished {
			trace.emit("context_done", diagnosticOutcome(ctx, nil, nil))
		}
	})
	return context.WithValue(ctx, requestTraceKey{}, trace), func(outcome string) {
		stop()
		if slow != nil {
			slow.Stop()
		}
		trace.mu.Lock()
		defer trace.mu.Unlock()
		if trace.finished {
			return
		}
		trace.finished = true
		trace.emit("finished", outcome)
	}
}

// Concurrent module workers must not overwrite their parent scan's single stage.
func withoutRequestTrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestTraceKey{}, struct{}{})
}

func logRequestPhase(ctx context.Context, phase string) {
	if trace, ok := ctx.Value(requestTraceKey{}).(*requestTrace); ok {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		if !trace.finished {
			trace.stage = phase
			trace.emit(phase, "")
		}
	}
}

func diagnosticOutcome(ctx context.Context, result mcp.Result, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if err != nil {
		return "error"
	}
	if call, ok := result.(*mcp.CallToolResult); ok && call != nil && call.IsError {
		return "tool_error"
	}
	return "ok"
}

func (a *App) requestDiagnosticsMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (result mcp.Result, err error) {
		label := "other"
		switch method {
		case "initialize", "discover", "ping", "tools/list", "tools/call", "resources/list", "resources/read", "resources/templates/list":
			label = method
		}
		tool := ""
		if call, ok := req.(*mcp.CallToolRequest); method == "tools/call" && ok && call != nil && call.Params != nil {
			tool = "other"
			if nativeToolAnnotations(call.Params.Name) != nil {
				tool = call.Params.Name
			}
		}
		ctx, finish := a.traceRequest(ctx, "rpc", label, tool)
		defer func() {
			if value := recover(); value != nil {
				finish("panic")
				panic(value)
			}
			finish(diagnosticOutcome(ctx, result, err))
		}()
		return next(ctx, method, req)
	}
}

func requestDispatchMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		logRequestPhase(ctx, "handler_start")
		return next(ctx, method, req)
	}
}

// Wrap authenticated MCP traffic without intercepting bodies or response writers.
// SDK flushing, session headers and streaming semantics remain unchanged.
func (a *App) requestHTTPDiagnostics(mode string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, finish := a.traceRequest(r.Context(), "http", mode, "")
		defer func() {
			if value := recover(); value != nil {
				finish("panic")
				panic(value)
			}
			outcome := diagnosticOutcome(ctx, nil, nil)
			if outcome == "ok" {
				outcome = "returned"
			}
			finish(outcome)
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
