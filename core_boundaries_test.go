package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func coreBoundaryServer(t *testing.T) (*App, *mcp.Server) {
	t.Helper()
	root := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{
		WorkRoot: root, StateDir: filepath.Join(root, "state"), MaxOutputBytes: 128 * 1024,
	}}}
	server := mcp.NewServer(&mcp.Implementation{Name: "core-boundary", Version: "1"}, nil)
	app.registerCore(server)
	return app, server
}

func coreBoundaryCall(t *testing.T, client *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s transport failed: %v", name, err)
	}
	return result
}

func TestCoreRememberReadErrorPreservesNotes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary-user file permission checks")
	}
	app, server := coreBoundaryServer(t)
	client := contractClient(t, server)
	path, err := app.scopeStatePath(nil, "unreadable", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"old\":\"KEEP_ORIGINAL\"}\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })
	if _, err := os.ReadFile(path); !os.IsPermission(err) {
		t.Fatalf("fixture must produce EACCES, got %v", err)
	}
	result := coreBoundaryCall(t, client, "remember", map[string]any{"scope": "unreadable", "key": "new", "value": "MUST_NOT_COMMIT"})
	if !result.IsError || !strings.Contains(toolText(result), "permission denied") {
		t.Errorf("unreadable notes must reject the update: error=%t output=%s", result.IsError, toolText(result))
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode().Perm() != 0000 {
		t.Error("read failure replaced or changed the original notes inode")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(original) {
		t.Errorf("read failure changed note bytes: %q, %v", got, err)
	}
}

func TestCoreRememberMissingNotesCanBeCreated(t *testing.T) {
	app, server := coreBoundaryServer(t)
	client := contractClient(t, server)
	path, err := app.existingScopeStatePath(nil, "new-notes", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fixture notes unexpectedly exist: %v", err)
	}
	result := coreBoundaryCall(t, client, "remember", map[string]any{"scope": "new-notes", "key": "first", "value": "CREATED"})
	if result.IsError {
		t.Fatalf("ENOENT should permit creation: %s", toolText(result))
	}
	got, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(got), `"first": "CREATED"`) {
		t.Fatalf("new notes not persisted: %q, %v", got, err)
	}
}

func TestCoreRememberNullStateIsPreserved(t *testing.T) {
	app, server := coreBoundaryServer(t)
	path, err := app.scopeStatePath(nil, "invalid-notes", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	result := coreBoundaryCall(t, contractClient(t, server), "remember", map[string]any{"scope": "invalid-notes", "key": "new", "value": "MUST_NOT_COMMIT"})
	if !result.IsError {
		t.Fatal("null notes state accepted")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !os.SameFile(before, after) || string(data) != "null\n" {
		t.Fatalf("invalid notes modified: %q %v", data, err)
	}
}

func coreBoundaryOpenFDs(path string) int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	count := 0
	for _, entry := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name())); err == nil && target == path {
			count++
		}
	}
	return count
}

// Cancel deterministically inside the real handler's high-offset scan, without
// changing its reader or relying on client-side cancellation returning early.
type coreBoundaryCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
	onScan func()
}

func (c *coreBoundaryCancelContext) Err() error {
	if c.checks.Add(1) == 64 {
		c.onScan()
		c.cancel()
	}
	return c.Context.Err()
}

func TestCoreReadFileCancellationAndPagination(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("FD closure assertion requires procfs")
	}
	for _, mode := range []string{"before_scan", "high_offset", "pagination"} {
		t.Run(mode, func(t *testing.T) {
			app, server := coreBoundaryServer(t)
			path := filepath.Join(app.config.Get().WorkRoot, "lines.txt")
			content := "one\ntwo\nthree\nfour\n"
			if mode == "high_offset" {
				content = strings.Repeat("line\n", 65536)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var during *coreBoundaryCancelContext
			var sawOpen atomic.Bool
			if mode == "before_scan" {
				cancel()
			} else if mode == "high_offset" {
				during = &coreBoundaryCancelContext{Context: ctx, cancel: cancel, onScan: func() {
					sawOpen.Store(coreBoundaryOpenFDs(path) == 1)
				}}
				ctx = during
			}
			server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(requestCtx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "tools/call" {
						requestCtx = ctx
					}
					return next(requestCtx, method, req)
				}
			})
			client := contractClient(t, server)
			args := map[string]any{"path": path, "limit": 2}
			if mode == "high_offset" {
				args["offset"] = 65535
			}
			started := time.Now()
			result := coreBoundaryCall(t, client, "read_file", args)
			if mode == "pagination" {
				if result.IsError || contractJSON(t, result.StructuredContent)["output"] != "1\tone\n2\ttwo\n\n(showing lines 1-2; request offset=2 for more)\n" {
					t.Fatalf("first page changed: %s", toolText(result))
				}
				args["offset"] = 2
				result = coreBoundaryCall(t, client, "read_file", args)
				if result.IsError || contractJSON(t, result.StructuredContent)["output"] != "3\tthree\n4\tfour\n" {
					t.Fatalf("last page changed: %s", toolText(result))
				}
			} else {
				if !result.IsError || !strings.Contains(toolText(result), context.Canceled.Error()) {
					t.Errorf("handler ignored server-side cancellation: %s", toolText(result))
				}
				if time.Since(started) > time.Second {
					t.Error("cancelled handler did not return promptly")
				}
				if during != nil && (!errors.Is(ctx.Err(), context.Canceled) || !sawOpen.Load()) {
					t.Error("cancellation did not occur inside an open-file scan")
				}
			}
			if count := coreBoundaryOpenFDs(path); count != 0 {
				t.Errorf("read_file retained %d file descriptors after return", count)
			}
		})
	}
}
