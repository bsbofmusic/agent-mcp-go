package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSessionHandlersCancellationRetainsSessionAndOutput(t *testing.T) {
	root := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, StateDir: root, MaxOutputBytes: 4096, CommandTimeoutSec: 5}}, sessions: NewSessionManager()}
	t.Cleanup(func() {
		if err := app.sessions.shutdown(); err != nil {
			t.Error(err)
		}
	})
	server := mcp.NewServer(&mcp.Implementation{Name: "handler-cancel", Version: "1"}, nil)
	app.registerCore(server)
	var mode atomic.Int32
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				if mode.Load() == 1 {
					child, cancel := context.WithCancel(ctx)
					cancel()
					return next(child, method, req)
				}
				if mode.Load() == 2 {
					child, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
					defer cancel()
					return next(child, method, req)
				}
			}
			return next(ctx, method, req)
		}
	})
	client := contractClient(t, server)
	mode.Store(1)
	result := coreBoundaryCall(t, client, "exec_command", map[string]any{"cmd": "printf MUST_NOT_START", "yield_time_ms": 1000})
	if !result.IsError {
		t.Fatal("pre-canceled exec succeeded")
	}
	app.sessions.mu.Lock()
	count := len(app.sessions.sessions)
	app.sessions.mu.Unlock()
	if count != 0 {
		t.Fatal("pre-canceled request launched a command")
	}
	mode.Store(2)
	result = coreBoundaryCall(t, client, "exec_command", map[string]any{"cmd": "printf BEFORE; read value; printf AFTER; sleep 3", "yield_time_ms": 30000})
	app.sessions.mu.Lock()
	var session *Session
	for _, s := range app.sessions.sessions {
		session = s
	}
	count = len(app.sessions.sessions)
	app.sessions.mu.Unlock()
	if count != 1 || session == nil || session.isFinished() {
		t.Fatal("canceled wait lost running session")
	}
	for _, fragment := range []string{fmt.Sprintf("session_id=%d", session.ID), "do not replay automatically"} {
		if !result.IsError || !strings.Contains(toolText(result), fragment) {
			t.Fatalf("missing cancellation recovery information: %s", toolText(result))
		}
	}
	result = coreBoundaryCall(t, client, "write_stdin", map[string]any{"session_id": session.ID, "chars": "ready\n", "yield_time_ms": 30000})
	if !result.IsError || !strings.Contains(toolText(result), "prior input may have been consumed") {
		t.Fatalf("lost input/replay warning after wait cancellation: %s", toolText(result))
	}
	if session.isFinished() {
		t.Fatal("input request cancellation killed session")
	}
	mode.Store(0)
	result = coreBoundaryCall(t, client, "write_stdin", map[string]any{"session_id": session.ID, "yield_time_ms": 10})
	if result.IsError || !strings.Contains(toolText(result), "BEFOREAFTER") {
		t.Fatalf("canceled polls consumed output: %s", toolText(result))
	}
}
