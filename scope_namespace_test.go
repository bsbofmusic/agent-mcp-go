package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestScopeMCPSessionAndExplicitNamespacesOverHTTP(t *testing.T) {
	for _, first := range []string{"automatic", "explicit"} {
		t.Run(first+"_writes_first", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			app, web, token := testApp(t)
			automatic := handoverHTTPClient(t, ctx, app, web, token, "/mcp")
			stateless := handoverHTTPClient(t, ctx, app, web, token, "/mcp/stateless")
			id := automatic.ID()
			identities := map[string]struct {
				client      *mcp.ClientSession
				scope, meta string
			}{
				"automatic": {client: automatic},
				"explicit":  {client: stateless, scope: id},
				"chatgpt":   {client: automatic, meta: id},
			}
			call := func(identity, tool string, args map[string]any) string {
				t.Helper()
				who := identities[identity]
				if who.scope != "" {
					args["scope"] = who.scope
				}
				return handoverOK(t, ctx, who.client, tool, args, who.meta)
			}
			write := func(identity, marker string) {
				t.Helper()
				call(identity, "remember", map[string]any{"key": "checkpoint", "value": marker + "_NOTE"})
				call(identity, "update_plan", map[string]any{"plan": []map[string]string{{"step": marker + "_PLAN", "status": "in_progress"}}})
			}
			assertState := func(identity, marker string) {
				t.Helper()
				text := call(identity, "recall", map[string]any{})
				var notes map[string]string
				if err := json.Unmarshal([]byte(text), &notes); err != nil {
					t.Fatalf("%s recall JSON: %v", identity, err)
				}
				brief := call(identity, "get_agent_brief", map[string]any{})
				if marker == "" {
					if len(notes) != 0 || !strings.Contains(brief, "No saved plan or notes exist for this task scope") {
						t.Errorf("empty %s identity consumed another namespace: notes=%v brief=%s", identity, notes, brief)
					}
					return
				}
				if len(notes) != 1 || notes["checkpoint"] != marker+"_NOTE" {
					t.Errorf("%s notes crossed namespaces: got %v want %s_NOTE", identity, notes, marker)
				}
				if !containsAll(brief, marker+"_NOTE", marker+"_PLAN") {
					t.Errorf("%s brief lost its own notes/plan: %s", identity, brief)
				}
				for _, other := range []string{"NS_AUTO_FIRST", "NS_AUTO_NEXT", "NS_EXPLICIT_FIRST", "NS_EXPLICIT_NEXT", "NS_CHATGPT"} {
					if other != marker && strings.Contains(brief, other) {
						t.Errorf("%s brief consumed %s from another namespace or overwritten state", identity, other)
					}
				}
			}
			second := "explicit"
			if first == "explicit" {
				second = "automatic"
			}
			markers := map[string]string{"automatic": "NS_AUTO_FIRST", "explicit": "NS_EXPLICIT_FIRST"}
			write(first, markers[first])
			assertState(second, "")
			assertState("chatgpt", "")
			write(second, markers[second])
			assertState(first, markers[first])
			assertState(second, markers[second])
			// Both directions must retain their own checkpoint and plan even when
			// the other identity overwrites the same key and the whole plan.
			for _, identity := range []string{first, second} {
				markers[identity] = strings.Replace(markers[identity], "FIRST", "NEXT", 1)
				write(identity, markers[identity])
				assertState("automatic", markers["automatic"])
				assertState("explicit", markers["explicit"])
			}
			write("chatgpt", "NS_CHATGPT")
			assertState("automatic", markers["automatic"])
			assertState("explicit", markers["explicit"])
			assertState("chatgpt", "NS_CHATGPT")
			// Deleting an explicit note may only affect its own namespace.
			call("explicit", "remember", map[string]any{"key": "checkpoint", "value": ""})
			assertState("automatic", markers["automatic"])
			assertState("chatgpt", "NS_CHATGPT")
		})
	}
}

func TestScopeHistoricalExplicitAndChatGPTKeysOverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	app, web, token := testApp(t)
	client := handoverHTTPClient(t, ctx, app, web, token, "/mcp/stateless")
	const id = "NS_HISTORY_1"
	// Freeze historical SHA-256(namespace+id)[:16] keys, independently of
	// the resolver. Existing explicit and ChatGPT records must stay readable
	// and writable in place, with no migration or alternate-key fallback.
	for _, fixture := range []struct {
		name, directory, scope, meta string
	}{
		{"explicit", "08667bea7349aa2ae1ba0140fd5ba835", id, ""},
		{"chatgpt", "460d82f611e1fd11547f741a5e3aa557", "", id},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := filepath.Join(app.config.Get().StateDir, "scopes", fixture.directory)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			marker := "NS_HISTORY_" + strings.ToUpper(fixture.name)
			files := map[string]string{
				"notes.json":       `{"checkpoint":"` + marker + `_NOTE"}`,
				"plan.json":        `{"plan":[{"step":"` + marker + `_PLAN","status":"in_progress"}]}`,
				"last-review.json": `{"historical-review":"keep-in-place"}`,
			}
			for name, raw := range files {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := map[string]any{}
			if fixture.scope != "" {
				args["scope"] = fixture.scope
			}
			notes := handoverOK(t, ctx, client, "recall", args, fixture.meta)
			brief := handoverOK(t, ctx, client, "get_agent_brief", args, fixture.meta)
			if !strings.Contains(notes, marker+"_NOTE") || !containsAll(brief, marker+"_NOTE", marker+"_PLAN") {
				t.Fatal("historical namespace key no longer restores its own state")
			}
			args["key"], args["value"] = "checkpoint", marker+"_NEXT_NOTE"
			handoverOK(t, ctx, client, "remember", args, fixture.meta)
			delete(args, "key")
			delete(args, "value")
			args["plan"] = []map[string]string{{"step": marker + "_NEXT_PLAN", "status": "completed"}}
			handoverOK(t, ctx, client, "update_plan", args, fixture.meta)
			for name, want := range map[string]string{"notes.json": marker + "_NEXT_NOTE", "plan.json": marker + "_NEXT_PLAN", "last-review.json": files["last-review.json"]} {
				raw, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil || !strings.Contains(string(raw), want) {
					t.Errorf("historical %s changed location or lost data: %s %v", name, raw, err)
				}
			}
		})
	}
	if directories := handoverScopeDirectories(t, app); len(directories) != 2 {
		t.Errorf("historical writes created alternate namespace directories: %v", directories)
	}
}
