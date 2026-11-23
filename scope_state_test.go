package main

import (
	"context"
	"encoding/json"
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

// recallSession exposes the core tools over the real MCP stack so the recall
// input schema itself is exercised, not only the Go handler.
func recallSession(t *testing.T, app *App) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "recall-filter-test", Version: "1"}, nil)
	app.registerCore(server)
	c, sv := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), sv)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "recall-filter-client", Version: "1"}, nil).
		Connect(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// recallOutputRaw returns the raw recall text from the structured output so the
// assertion is independent of which transport path rendered the TextContent.
func recallOutputRaw(t *testing.T, session *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "recall", Arguments: args})
	if err != nil {
		t.Fatalf("recall transport: %v", err)
	}
	if res.IsError {
		t.Fatalf("recall is an error result: %s", toolText(res))
	}
	encoded, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out TextOut
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("recall structuredContent is not a TextOut: %v (%s)", err, encoded)
	}
	return out.Output
}

func recallJSON(t *testing.T, session *mcp.ClientSession, args map[string]any) map[string]string {
	t.Helper()
	text := strings.TrimSpace(recallOutputRaw(t, session, args))
	notes := map[string]string{}
	if err := json.Unmarshal([]byte(text), &notes); err != nil {
		t.Fatalf("recall output is not a JSON object: %v (%q)", err, text)
	}
	return notes
}

// TestRecallKeyAndPrefixFiltering pins the declarative recall filter contract:
// an exact key, a key prefix, no match, the unfiltered default, and the
// precedence of an exact key over a prefix.
func TestRecallKeyAndPrefixFiltering(t *testing.T) {
	app, _, _ := testApp(t)
	session := recallSession(t, app)
	const scope = "recall-filter-scope"
	write := func(key, value string) {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"scope": scope, "key": key, "value": value,
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember %s: err=%v res=%s", key, err, toolText(res))
		}
	}
	write("alpha", "one")
	write("beta", "two")
	write("checkpoint-1", "step one")
	write("checkpoint-2", "step two")

	t.Run("default_returns_all", func(t *testing.T) {
		got := recallJSON(t, session, map[string]any{"scope": scope})
		if len(got) != 4 || got["alpha"] != "one" || got["checkpoint-2"] != "step two" {
			t.Fatalf("unfiltered recall = %v, want all four notes", got)
		}
	})
	t.Run("exact_key", func(t *testing.T) {
		got := recallJSON(t, session, map[string]any{"scope": scope, "key": "alpha"})
		if len(got) != 1 || got["alpha"] != "one" {
			t.Fatalf("exact key recall = %v, want only alpha", got)
		}
	})
	t.Run("prefix", func(t *testing.T) {
		got := recallJSON(t, session, map[string]any{"scope": scope, "prefix": "checkpoint-"})
		if len(got) != 2 || got["checkpoint-1"] != "step one" || got["checkpoint-2"] != "step two" {
			t.Fatalf("prefix recall = %v, want both checkpoints", got)
		}
	})
	t.Run("no_match_is_empty_object", func(t *testing.T) {
		if text := strings.TrimSpace(recallOutputRaw(t, session, map[string]any{"scope": scope, "prefix": "absent-"})); text != "{}" {
			t.Fatalf("no-match recall = %q, want {}", text)
		}
	})
	t.Run("key_wins_over_prefix", func(t *testing.T) {
		got := recallJSON(t, session, map[string]any{"scope": scope, "key": "alpha", "prefix": "checkpoint-"})
		if len(got) != 1 || got["alpha"] != "one" {
			t.Fatalf("key with prefix recall = %v, want only alpha", got)
		}
	})
	t.Run("indented_json", func(t *testing.T) {
		text := recallOutputRaw(t, session, map[string]any{"scope": scope, "prefix": "checkpoint-"})
		if !strings.Contains(text, "\n  \"checkpoint-1\": \"step one\",") {
			t.Fatalf("recall output lost its two-space indented JSON shape: %q", text)
		}
	})
	t.Run("scopes_stay_isolated", func(t *testing.T) {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{
			"scope": scope + "-other", "key": "alpha", "value": "other-scope",
		}})
		if err != nil || res.IsError {
			t.Fatalf("remember in other scope: err=%v res=%s", err, toolText(res))
		}
		if got := recallJSON(t, session, map[string]any{"scope": scope, "key": "alpha"}); len(got) != 1 || got["alpha"] != "one" {
			t.Fatalf("filtered recall crossed scopes: %v", got)
		}
	})
}
