package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestScopedPlanAndNotesIsolation(t *testing.T) {
	app, _, _ := testApp(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "scope-test", Version: "1"}, nil)
	app.registerCore(server)
	app.registerParity(server)
	c, sv := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), sv)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "scope-client", Version: "1"}, nil).Connect(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		r, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil {
			t.Fatalf("%s transport: %v", name, e)
		}
		return toolText(r), r.IsError
	}
	if _, bad := call("remember", map[string]any{"scope": "chat-a", "key": "secret", "value": "alpha"}); bad {
		t.Fatal("remember chat-a failed")
	}
	if _, bad := call("remember", map[string]any{"scope": "chat-b", "key": "secret", "value": "beta"}); bad {
		t.Fatal("remember chat-b failed")
	}
	a, bad := call("recall", map[string]any{"scope": "chat-a"})
	if bad || !containsAll(a, "alpha", "secret") {
		t.Fatal("chat-a note inaccessible")
	}
	b, bad := call("recall", map[string]any{"scope": "chat-b"})
	if bad || !containsAll(b, "beta", "secret") {
		t.Fatal("chat-b note inaccessible")
	}
	if containsAll(a, "beta") || containsAll(b, "alpha") {
		t.Fatal("notes crossed scopes")
	}
	_, bad = call("update_plan", map[string]any{"scope": "chat-a", "plan": []map[string]string{{"step": "Alpha", "status": "pending"}}})
	if bad {
		t.Fatal("plan-a failed")
	}
	_, bad = call("update_plan", map[string]any{"scope": "chat-b", "plan": []map[string]string{{"step": "Beta", "status": "completed"}}})
	if bad {
		t.Fatal("plan-b failed")
	}
	pa, e := app.scopeStatePath(nil, "chat-a", "plan.json")
	if e != nil {
		t.Fatal(e)
	}
	pb, e := app.scopeStatePath(nil, "chat-b", "plan.json")
	if e != nil {
		t.Fatal(e)
	}
	ba, _ := os.ReadFile(pa)
	bb, _ := os.ReadFile(pb)
	if string(ba) == string(bb) || !containsAll(string(ba), "Alpha") || !containsAll(string(bb), "Beta") {
		t.Fatal("plan states overlapped")
	}
	// CodexFree's build_instructions includes saved task state; AMG can
	// render the same handover for an explicit stable task scope.
	briefA, bad := call("get_agent_brief", map[string]any{"scope": "chat-a"})
	if bad || !containsAll(briefA, "Saved state", "alpha", "Alpha") || strings.Contains(briefA, "Beta") || strings.Contains(briefA, "beta") {
		t.Fatal("task-a handover missing data or leaked task-b notes")
	}
	briefB, bad := call("get_agent_brief", map[string]any{"scope": "chat-b"})
	if bad || !containsAll(briefB, "Saved state", "beta", "Beta") || strings.Contains(briefB, "Alpha") || strings.Contains(briefB, "alpha") {
		t.Fatal("task-b handover missing data or leaked task-a notes")
	}
	briefUnknown, bad := call("get_agent_brief", map[string]any{"scope": "new-task"})
	if bad || strings.Contains(briefUnknown, "Earlier task-local notes") {
		t.Fatal("new-task brief loaded unrelated saved state")
	}
	unknownPath, err := app.existingScopeStatePath(nil, "new-task", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(unknownPath)); !os.IsNotExist(err) {
		t.Fatalf("read-only task bootstrap created an unused state directory: %v", err)
	}
	for _, scope := range []string{"chat-a", "chat-b"} {
		p, e := app.scopeStatePath(nil, scope, "last-review.json")
		if e != nil {
			t.Fatal(e)
		}
		if filepath.Dir(p) == app.config.Get().StateDir {
			t.Fatal("review state not isolated")
		}
	}
}

func containsAll(source string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(source, part) {
			return false
		}
	}
	return true
}
