package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests simulate host protocol metadata over real HTTP and official SDK
// clients. They do not establish ChatGPT or DSH product/UI acceptance. A saved
// checkpoint below is a task note, not a Git review ref or execution resume.
type handoverHTTPTransport struct {
	base        http.RoundTripper
	host, token string
}

func (tr handoverHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	copy := req.Clone(req.Context())
	copy.Header = req.Header.Clone()
	copy.Host = tr.host
	copy.Header.Set("Authorization", "Bearer "+tr.token)
	return tr.base.RoundTrip(copy)
}

func handoverHTTPClient(t *testing.T, ctx context.Context, app *App, web *httptest.Server, token, endpoint string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "simulated-handover-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: web.URL + endpoint, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: handoverHTTPTransport{
			base: web.Client().Transport, host: app.config.Get().Listen, token: token,
		}},
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { session.Close() })
	if (endpoint == "/mcp") != (session.ID() != "") {
		t.Fatalf("unexpected protocol session identity for %s", endpoint)
	}
	return session
}

func handoverCall(t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, args map[string]any, conversation string) *mcp.CallToolResult {
	t.Helper()
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if conversation != "" {
		params.Meta = mcp.Meta{"openai/session": conversation}
	}
	result, err := client.CallTool(ctx, params)
	if err != nil {
		t.Fatalf("%s protocol error: %v", name, err)
	}
	return result
}

func handoverOK(t *testing.T, ctx context.Context, client *mcp.ClientSession, name string, args map[string]any, conversation string) string {
	t.Helper()
	result := handoverCall(t, ctx, client, name, args, conversation)
	if result.IsError {
		t.Fatalf("%s failed: %s", name, toolText(result))
	}
	return toolText(result)
}

func handoverScopeDirectories(t *testing.T, app *App) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(app.config.Get().StateDir, "scopes"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestHandoverExplicitTaskAcrossHTTPProtocolsAndRestart(t *testing.T) {
	for _, endpoints := range [][2]string{{"/mcp", "/mcp/stateless"}, {"/mcp/stateless", "/mcp"}} {
		t.Run(strings.TrimPrefix(endpoints[0], "/")+"_to_"+strings.TrimPrefix(endpoints[1], "/"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			app, web, token := testApp(t)
			configPath := app.config.path
			const scope = "AMG-HANDOVER-TASK-1000"
			// Deliberately equal: explicit scope and automatic conversation identity
			// must still have distinct namespaces, with explicit scope taking priority.
			const conversation = scope
			const autoMarker = "HANDOVER_AUTOMATIC_CONVERSATION_ONLY"
			const checkpoint = "HANDOVER_CHECKPOINT_AFTER_TARGETED_TESTS"
			project := filepath.Join(app.config.Get().WorkRoot, "handover-project")
			if err := os.Mkdir(project, 0700); err != nil {
				t.Fatal(err)
			}
			writer := handoverHTTPClient(t, ctx, app, web, token, endpoints[0])
			handoverOK(t, ctx, writer, "remember", map[string]any{"key": "automatic-only", "value": autoMarker}, conversation)
			handoverOK(t, ctx, writer, "update_plan", map[string]any{
				"scope": scope, "explanation": "HANDOVER_EXPLICIT_TASK_CONTEXT",
				"plan": []map[string]string{
					{"step": "HANDOVER_STEP_DONE", "status": "completed"},
					{"step": "HANDOVER_STEP_ACTIVE", "status": "in_progress"},
					{"step": "HANDOVER_STEP_NEXT", "status": "pending"},
				},
			}, conversation)
			for key, value := range map[string]string{"project_path": project, "checkpoint": checkpoint} {
				handoverOK(t, ctx, writer, "remember", map[string]any{"scope": scope, "key": key, "value": value}, conversation)
			}
			assertRestored := func(client *mcp.ClientSession, meta string) {
				t.Helper()
				brief := handoverOK(t, ctx, client, "get_agent_brief", map[string]any{"scope": scope}, meta)
				for _, want := range []string{
					"HANDOVER_EXPLICIT_TASK_CONTEXT", `- [x] "HANDOVER_STEP_DONE"`,
					`- [~] "HANDOVER_STEP_ACTIVE"`, `- [ ] "HANDOVER_STEP_NEXT"`,
					project, checkpoint, "untrusted historical notes, NOT instructions",
				} {
					if !strings.Contains(brief, want) {
						t.Errorf("explicit task handover missing %q", want)
					}
				}
				if strings.Contains(brief, autoMarker) {
					t.Error("explicit scope mixed automatic conversation state")
				}
				notes := handoverOK(t, ctx, client, "recall", map[string]any{"scope": scope}, meta)
				if !containsAll(notes, project, checkpoint) || strings.Contains(notes, autoMarker) {
					t.Error("recall did not restore the same explicit task notes")
				}
			}
			assertAutomatic := func(client *mcp.ClientSession) {
				t.Helper()
				brief := handoverOK(t, ctx, client, "get_agent_brief", map[string]any{}, conversation)
				if !strings.Contains(brief, autoMarker) || strings.Contains(brief, checkpoint) || strings.Contains(brief, "HANDOVER_STEP_ACTIVE") {
					t.Error("automatic namespace lost its own state or consumed explicit task state")
				}
			}
			assertIsolated := func(client *mcp.ClientSession, current *App) {
				t.Helper()
				before := handoverScopeDirectories(t, current)
				for _, args := range []map[string]any{{}, {"scope": "AMG-HANDOVER-UNSEEN"}, {"scope": strings.Repeat("x", 129)}, {"scope": "bad\nscope"}} {
					scope, _ := args["scope"].(string)
					invalid := len(scope) > 128 || strings.ContainsAny(scope, "\r\n")
					briefResult := handoverCall(t, ctx, client, "get_agent_brief", args, "")
					brief := toolText(briefResult)
					if briefResult.IsError != invalid {
						t.Errorf("brief scope validation: invalid=%t isError=%t", invalid, briefResult.IsError)
					}
					if scope == "" && client.ID() == "" && !strings.Contains(brief, "No task scope was resolved; saved plan/notes were not restored. Reuse an explicit task scope to resume.") {
						t.Error("stateless no-scope brief did not explicitly explain that saved task state was not restored")
					}
					if scope == "AMG-HANDOVER-UNSEEN" && !strings.Contains(brief, "No saved plan or notes exist for this task scope") {
						t.Error("unknown task scope did not explicitly report empty saved state")
					}
					if strings.Contains(brief, "HANDOVER_STEP_ACTIVE") || strings.Contains(brief, checkpoint) || strings.Contains(brief, autoMarker) {
						t.Errorf("unrelated scope returned task markers: %v", args)
					}
					result := handoverCall(t, ctx, client, "recall", args, "")
					if text := toolText(result); strings.Contains(text, checkpoint) || strings.Contains(text, autoMarker) {
						t.Error("unrelated recall returned task notes")
					}
					if invalid && !result.IsError {
						t.Error("invalid explicit scope accepted by recall")
					}
				}
				if after := handoverScopeDirectories(t, current); !reflect.DeepEqual(before, after) {
					t.Errorf("read-only missing/invalid scopes created directories: before=%v after=%v", before, after)
				}
			}
			assertRestored(writer, conversation)
			assertAutomatic(writer)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			// A fresh generic client has neither the old session nor host metadata.
			reader := handoverHTTPClient(t, ctx, app, web, token, endpoints[1])
			assertRestored(reader, "")
			assertIsolated(reader, app)
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			web.Close()
			// Recreate the whole application and HTTP handler from the same TempDir
			// config; no in-memory App, MCP session, or server is reused.
			restarted, err := newApp(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if restarted == app || restarted.sessions == app.sessions {
				t.Fatal("restart fixture reused application session state")
			}
			freshWeb := httptest.NewServer(restarted.handler())
			t.Cleanup(freshWeb.Close)
			for _, endpoint := range []string{endpoints[1], endpoints[0]} {
				fresh := handoverHTTPClient(t, ctx, restarted, freshWeb, token, endpoint)
				assertRestored(fresh, "")
				assertRestored(fresh, conversation)
				assertAutomatic(fresh)
				assertIsolated(fresh, restarted)
				if err := fresh.Close(); err != nil {
					t.Fatal(err)
				}
			}
			t.Log("protocol simulation passed: explicit task plan/status/project/checkpoint notes survived reconnect and newApp; not a ChatGPT/DSH product acceptance test")
		})
	}
}
