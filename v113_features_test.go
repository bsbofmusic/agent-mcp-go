package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// v113OriginServer starts an authenticated HTTP server whose public URL
// matches the listener address, so the Host check passes and the Origin
// policy is the only remaining browser-side restriction.
func v113OriginServer(t *testing.T, mutate func(*Config)) (*App, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	token := strings.Repeat("v", 48)
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	cfg := Config{
		Listen:            address,
		WorkRoot:          dir,
		SkillsRoot:        filepath.Join(dir, "skills"),
		StateDir:          filepath.Join(dir, "state"),
		TokenFile:         tokenPath,
		PublicURL:         "http://" + address,
		MaxFileBytes:      300 * 1024 * 1024,
		MaxOutputBytes:    128 * 1024,
		CommandTimeoutSec: 60,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: app.handler()}}
	server.Start()
	t.Cleanup(server.Close)
	return app, server, token
}

// v113Post sends one authenticated or anonymous request and returns the
// status code and body.
func v113PostFull(t *testing.T, server *httptest.Server, token, origin string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, 512)
	n, _ := response.Body.Read(body)
	return response, string(body[:n])
}

func v113Post(t *testing.T, server *httptest.Server, token, origin string) (int, string) {
	t.Helper()
	request, err := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, 512)
	n, _ := response.Body.Read(body)
	return response.StatusCode, string(body[:n])
}

// TestV113AllowedOriginsValidation covers the configuration contract of the
// remote client origin allowlist.
func TestV113AllowedOriginsValidation(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	base := baseTestConfig(t, root, root)

	accepted := []string{
		"https://claude.ai",
		"http://localhost:3000",
		"https://mcp.example.com",
		"app://obsidian.md",
		"vscode-webview://obsidian.md",
		"tauri://localhost",
		"*",
	}
	for _, origin := range accepted {
		cfg := base
		cfg.AllowedOrigins = []string{origin}
		writeTestConfig(t, cfgPath, cfg)
		got, err := readConfig(cfgPath)
		if err != nil {
			t.Fatalf("allowed_origins %q was rejected: %v", origin, err)
		}
		if len(got.AllowedOrigins) != 1 || got.AllowedOrigins[0] != origin {
			t.Fatalf("allowed_origins %q was not stored verbatim: %v", origin, got.AllowedOrigins)
		}
	}

	// Entries are trimmed before they are stored.
	cfg := base
	cfg.AllowedOrigins = []string{"  https://claude.ai  "}
	writeTestConfig(t, cfgPath, cfg)
	got, err := readConfig(cfgPath)
	if err != nil {
		t.Fatalf("padded origin was rejected: %v", err)
	}
	if got.AllowedOrigins[0] != "https://claude.ai" {
		t.Fatalf("allowed_origins = %v, want the trimmed origin", got.AllowedOrigins)
	}

	rejected := []struct {
		name   string
		origin string
	}{
		{"empty entry", ""},
		{"blank entry", "   "},
		{"trailing slash", "https://claude.ai/"},
		{"path", "https://claude.ai/mcp"},
		{"query", "https://claude.ai?token=1"},
		{"fragment", "https://claude.ai#section"},
		{"missing scheme", "claude.ai"},
		{"missing host", "https://"},
		{"credentials", "https://user:secret@claude.ai"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.AllowedOrigins = []string{tc.origin}
			writeTestConfig(t, cfgPath, cfg)
			if _, err := readConfig(cfgPath); err == nil {
				t.Fatalf("allowed_origins %q was accepted", tc.origin)
			}
		})
	}
}

// TestV113OriginPolicy covers the runtime origin policy: the public origin
// stays allowed, unknown origins are refused, configured remote client
// origins are accepted, and authentication always comes first.
func TestV113OriginPolicy(t *testing.T) {
	t.Run("default policy refuses unknown origins", func(t *testing.T) {
		_, server, token := v113OriginServer(t, nil)
		status, body := v113Post(t, server, token, "https://evil.invalid")
		if status != http.StatusForbidden || !strings.Contains(body, "origin denied") {
			t.Fatalf("unknown origin: status=%d body=%q, want 403 origin denied", status, body)
		}
		// The public origin itself stays allowed.
		status, body = v113Post(t, server, token, "http://"+server.Listener.Addr().String())
		if status == http.StatusForbidden || strings.Contains(body, "origin denied") {
			t.Fatalf("public origin refused: status=%d body=%q", status, body)
		}
	})

	t.Run("configured remote client origin is accepted", func(t *testing.T) {
		_, server, token := v113OriginServer(t, func(cfg *Config) {
			cfg.AllowedOrigins = []string{"https://claude.ai", "app://obsidian.md"}
		})
		for _, origin := range []string{"https://claude.ai", "app://obsidian.md"} {
			status, body := v113Post(t, server, token, origin)
			if status == http.StatusForbidden || strings.Contains(body, "origin denied") {
				t.Fatalf("allowed origin %q refused: status=%d body=%q", origin, status, body)
			}
		}
		// An origin that is not on the allowlist stays refused.
		status, body := v113Post(t, server, token, "https://evil.invalid")
		if status != http.StatusForbidden || !strings.Contains(body, "origin denied") {
			t.Fatalf("unknown origin: status=%d body=%q, want 403 origin denied", status, body)
		}
	})

	t.Run("wildcard allows any origin", func(t *testing.T) {
		_, server, token := v113OriginServer(t, func(cfg *Config) {
			cfg.AllowedOrigins = []string{"*"}
		})
		status, body := v113Post(t, server, token, "https://any-client.invalid")
		if status == http.StatusForbidden || strings.Contains(body, "origin denied") {
			t.Fatalf("wildcard origin refused: status=%d body=%q", status, body)
		}
	})

	t.Run("authentication stays in front of the origin check", func(t *testing.T) {
		_, server, _ := v113OriginServer(t, func(cfg *Config) {
			cfg.AllowedOrigins = []string{"*"}
		})
		for _, origin := range []string{"https://claude.ai", "https://evil.invalid"} {
			response, body := v113PostFull(t, server, "", origin)
			if response.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(response.Header.Get("WWW-Authenticate"), "Bearer") {
				t.Fatalf("unauthenticated request with origin %q: status=%d body=%q challenge=%q, want 401 with a Bearer challenge",
					origin, response.StatusCode, body, response.Header.Get("WWW-Authenticate"))
			}
		}
		status, body := v113Post(t, server, "wrong-token", "https://claude.ai")
		if status != http.StatusUnauthorized {
			t.Fatalf("invalid token: status=%d body=%q, want 401", status, body)
		}
	})
}

// TestV113AllowsOriginHelper pins the origin helper contract.
func TestV113AllowsOriginHelper(t *testing.T) {
	cfg := Config{PublicURL: "https://mcp.example.com/", AllowedOrigins: []string{"https://claude.ai"}}
	if !cfg.allowsOrigin("") {
		t.Error("an empty origin must stay allowed for non-browser clients")
	}
	if !cfg.allowsOrigin("https://mcp.example.com") {
		t.Error("the public origin must stay allowed")
	}
	if !cfg.allowsOrigin("https://claude.ai") {
		t.Error("a configured origin must be allowed")
	}
	if cfg.allowsOrigin("https://evil.invalid") {
		t.Error("an unknown origin must be refused")
	}
	wildcard := Config{PublicURL: "https://mcp.example.com", AllowedOrigins: []string{"*"}}
	if !wildcard.allowsOrigin("https://anything.invalid") {
		t.Error("the wildcard must allow every origin")
	}
}

// TestV113RunCommandWorkdir covers the optional working directory of
// run_command: the default working directory, explicit relative and absolute
// directories, and the refusals outside work_root or on a regular file.
func TestV113RunCommandWorkdir(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required for the working-directory fixture")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	sub := filepath.Join(project, "sub")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{project, sub} {
		command := exec.Command(gitPath, "init", "-q", dir)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git init %s failed: %v %s", dir, err, out)
		}
	}
	regularFile := filepath.Join(project, "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	app := newTestAppWithConfig(t, baseTestConfig(t, root, project))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	toplevel := func(out TextOut) string {
		line := strings.TrimSpace(strings.SplitN(out.Output, "\n", 2)[0])
		resolved, err := filepath.EvalSymlinks(line)
		if err != nil {
			return line
		}
		return resolved
	}
	run := func(workdir string) (TextOut, bool, error) {
		return app.parityRunCommand(ctx, RunCommandInput{Command: "git", Args: []string{"rev-parse", "--show-toplevel"}, Workdir: workdir})
	}
	wantProject, _ := filepath.EvalSymlinks(project)
	wantSub, _ := filepath.EvalSymlinks(sub)

	// Without a workdir the command runs in the default working directory.
	out, failed, err := run("")
	if err != nil || failed {
		t.Fatalf("run_command without workdir failed: %v %s", err, out.Output)
	}
	if got := toplevel(out); got != wantProject {
		t.Fatalf("default working directory = %q, want %q", got, wantProject)
	}
	// A relative workdir resolves against the default working directory.
	out, failed, err = run("sub")
	if err != nil || failed {
		t.Fatalf("relative run_command workdir failed: %v %s", err, out.Output)
	}
	if got := toplevel(out); got != wantSub {
		t.Fatalf("relative working directory = %q, want %q", got, wantSub)
	}
	// An absolute in-work_root workdir is honoured.
	out, failed, err = run(sub)
	if err != nil || failed {
		t.Fatalf("absolute run_command workdir failed: %v %s", err, out.Output)
	}
	if got := toplevel(out); got != wantSub {
		t.Fatalf("absolute working directory = %q, want %q", got, wantSub)
	}
	// A workdir outside work_root stays refused.
	if _, _, err := run("../.."); err == nil {
		t.Fatal("run_command accepted a workdir outside work_root")
	}
	// A regular file is not a working directory.
	if _, _, err := run(regularFile); err == nil || !strings.Contains(err.Error(), "workdir is not a directory") {
		t.Fatalf("run_command accepted a regular file as workdir: %v", err)
	}
	// A missing directory is refused as well.
	if _, _, err := run("missing-directory"); err == nil {
		t.Fatal("run_command accepted a missing workdir")
	}
}

// TestV113ConcurrentScopes proves that concurrent agents writing task state
// never block each other across scopes and never contaminate each other.
func TestV113ConcurrentScopes(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))

	const scopes = 10
	const workers = 4
	failures := make(chan string, scopes*workers*4)
	var wg sync.WaitGroup
	for i := 0; i < scopes; i++ {
		scope := fmt.Sprintf("agent-%02d", i)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(scope string, worker int) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				key := fmt.Sprintf("key-%d", worker)
				if _, err := app.rememberNote(ctx, nil, NoteInput{Scope: scope, Key: key, Value: scope + ":" + key}); err != nil {
					failures <- fmt.Sprintf("remember %s/%s: %v", scope, key, err)
					return
				}
				if _, err := app.recallNotes(ctx, nil, RecallInput{Scope: scope}); err != nil {
					failures <- fmt.Sprintf("recall %s: %v", scope, err)
					return
				}
				step := scope + " step " + key
				if _, err := app.parityUpdatePlan(ctx, nil, PlanInput{Scope: scope, Plan: []PlanStep{{Step: step, Status: "in_progress"}}}); err != nil {
					failures <- fmt.Sprintf("update_plan %s: %v", scope, err)
					return
				}
				if _, err := app.rememberNote(ctx, nil, NoteInput{Scope: scope, Key: key, Value: ""}); err != nil {
					failures <- fmt.Sprintf("delete %s/%s: %v", scope, key, err)
					return
				}
			}(scope, w)
		}
	}
	wg.Wait()
	close(failures)
	for message := range failures {
		t.Error(message)
	}
	if t.Failed() {
		return
	}

	for i := 0; i < scopes; i++ {
		scope := fmt.Sprintf("agent-%02d", i)
		notes, err := app.recallNotes(context.Background(), nil, RecallInput{Scope: scope})
		if err != nil {
			t.Fatalf("recall %s failed: %v", scope, err)
		}
		for j := 0; j < scopes; j++ {
			other := fmt.Sprintf("agent-%02d", j)
			if other == scope {
				continue
			}
			if strings.Contains(notes.Output, other+":") {
				t.Fatalf("scope %s contains notes of %s: %s", scope, other, notes.Output)
			}
		}
		if strings.Contains(notes.Output, scope+":") {
			t.Fatalf("scope %s lost the empty-value deletion of its own keys: %s", scope, notes.Output)
		}
		planPath, err := app.existingScopeStatePath(nil, scope, "plan.json")
		if err != nil {
			t.Fatalf("plan path %s failed: %v", scope, err)
		}
		plan, err := os.ReadFile(planPath)
		if err != nil {
			t.Fatalf("plan %s unreadable: %v", scope, err)
		}
		if !strings.Contains(string(plan), scope+" step ") {
			t.Fatalf("plan %s lost its own step: %s", scope, string(plan))
		}
		for j := 0; j < scopes; j++ {
			other := fmt.Sprintf("agent-%02d", j)
			if other == scope {
				continue
			}
			if strings.Contains(string(plan), other+" step ") {
				t.Fatalf("plan %s contains the step of %s: %s", scope, other, string(plan))
			}
		}
	}

	// The per-scope lock pool keeps no idle entries after the run.
	app.stateGuards.mu.Lock()
	idle := len(app.stateGuards.locks)
	app.stateGuards.mu.Unlock()
	if idle != 0 {
		t.Fatalf("scope lock pool retained %d idle locks", idle)
	}
}

// TestV113ConcurrentSameScope proves that concurrent writers of one scope
// lose no update and that the scope lock serializes them.
func TestV113ConcurrentSameScope(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	const writers = 16
	var wg sync.WaitGroup
	failures := make(chan string, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := app.rememberNote(ctx, nil, NoteInput{Scope: "shared", Key: fmt.Sprintf("key-%02d", worker), Value: fmt.Sprintf("value-%02d", worker)}); err != nil {
				failures <- fmt.Sprintf("remember %d: %v", worker, err)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for message := range failures {
		t.Error(message)
	}
	if t.Failed() {
		return
	}
	notes, err := app.recallNotes(context.Background(), nil, RecallInput{Scope: "shared"})
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	for i := 0; i < writers; i++ {
		key := fmt.Sprintf("key-%02d", i)
		if !strings.Contains(notes.Output, key) {
			t.Fatalf("concurrent write lost %s: %s", key, notes.Output)
		}
	}

	// Concurrent deletions of half of the keys keep the other half intact.
	var wg2 sync.WaitGroup
	failures2 := make(chan string, writers)
	for i := 0; i < writers/2; i++ {
		wg2.Add(1)
		go func(worker int) {
			defer wg2.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if _, err := app.rememberNote(ctx, nil, NoteInput{Scope: "shared", Key: fmt.Sprintf("key-%02d", worker), Value: ""}); err != nil {
				failures2 <- fmt.Sprintf("delete %d: %v", worker, err)
			}
		}(i)
	}
	wg2.Wait()
	close(failures2)
	for message := range failures2 {
		t.Error(message)
	}
	notes, err = app.recallNotes(context.Background(), nil, RecallInput{Scope: "shared"})
	if err != nil {
		t.Fatalf("recall after deletions failed: %v", err)
	}
	for i := 0; i < writers/2; i++ {
		if strings.Contains(notes.Output, fmt.Sprintf("key-%02d", i)) {
			t.Fatalf("deleted key key-%02d survived: %s", i, notes.Output)
		}
	}
	for i := writers / 2; i < writers; i++ {
		if !strings.Contains(notes.Output, fmt.Sprintf("key-%02d", i)) {
			t.Fatalf("surviving key key-%02d was lost: %s", i, notes.Output)
		}
	}
}

// TestV113RememberDeletesWithEmptyValue proves that the public remember tool
// deletes exactly one task-local note key when the value is empty, without
// adding a separate tool to the top-level surface.
func TestV113RememberDeletesWithEmptyValue(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	server := mcp.NewServer(&mcp.Implementation{Name: "v113-remember-delete", Version: "1"}, nil)
	app.registerCore(server)
	client := contractClient(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s call failed: %v", name, err)
		}
		return result
	}
	// The v1.1.2 top-level surface is preserved: deletion rides on remember.
	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}
	names := map[string]bool{}
	descriptions := map[string]string{}
	for _, tool := range listed.Tools {
		names[tool.Name] = true
		descriptions[tool.Name] = tool.Description
	}
	if names["forget"] {
		t.Fatal("the extra forget tool must not be registered")
	}
	if !names["remember"] || !names["recall"] {
		t.Fatalf("remember/recall missing from the tool surface: %v", names)
	}
	if !strings.Contains(descriptions["remember"], "empty value") {
		t.Fatalf("remember description does not document empty-value deletion: %s", descriptions["remember"])
	}

	if result := call("remember", map[string]any{"scope": "task-a", "key": "alpha", "value": "ALPHA"}); result.IsError {
		t.Fatalf("remember failed: %s", toolText(result))
	}
	if result := call("remember", map[string]any{"scope": "task-a", "key": "beta", "value": "BETA"}); result.IsError {
		t.Fatalf("remember failed: %s", toolText(result))
	}
	result := call("remember", map[string]any{"scope": "task-a", "key": "alpha", "value": ""})
	if result.IsError {
		t.Fatalf("empty-value deletion failed: %v %s", result.IsError, toolText(result))
	}
	notes := call("recall", map[string]any{"scope": "task-a"})
	if notes.IsError {
		t.Fatalf("recall failed: %s", toolText(notes))
	}
	if strings.Contains(toolText(notes), "ALPHA") || !strings.Contains(toolText(notes), "BETA") {
		t.Fatalf("empty-value deletion did not remove exactly one note: %s", toolText(notes))
	}
	// Deleting an unknown key is a no-op, not an error.
	result = call("remember", map[string]any{"scope": "task-a", "key": "missing", "value": ""})
	if result.IsError {
		t.Fatalf("empty-value deletion of an unknown key failed: %v %s", result.IsError, toolText(result))
	}
	// A blank key stays invalid, with or without a value.
	result = call("remember", map[string]any{"scope": "task-a", "key": "", "value": ""})
	if !result.IsError {
		t.Fatal("remember accepted an empty key")
	}
}

// TestV113EphemeralSessionMarker proves that only anonymous MCP session
// scopes carry the ephemeral marker and that durable scopes never do.
func TestV113EphemeralSessionMarker(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	// The background sweep stays disabled during the assertions.
	app.scopeSweepAt.Store(time.Now().UnixNano())

	explicitPath, err := app.scopeStatePath(nil, "durable-explicit", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	chatGPTRequest := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"openai/session": "conv-durable"}}}
	chatGPTPath, err := app.scopeStatePath(chatGPTRequest, "", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{explicitPath, chatGPTPath} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), ephemeralSessionMarker)); !os.IsNotExist(err) {
			t.Fatalf("durable scope %s carries the ephemeral marker: %v", filepath.Dir(path), err)
		}
	}

	tokenBytes, err := os.ReadFile(app.config.Get().TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(app.handler())
	t.Cleanup(web.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, app, web, strings.TrimSpace(string(tokenBytes)), "/mcp")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"key": "session-note", "value": "SESSION"}})
	if err != nil {
		t.Fatalf("remember call failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("remember failed: %s", toolText(result))
	}
	scopes := filepath.Join(app.config.Get().StateDir, "scopes")
	entries, err := os.ReadDir(scopes)
	if err != nil {
		t.Fatalf("scope directory unreadable: %v", err)
	}
	marked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(scopes, entry.Name(), ephemeralSessionMarker)); err == nil {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("marked scope directories = %d, want exactly one anonymous session scope", marked)
	}
}

// TestV113ReadRefreshesEphemeralMarker proves that a read-only recall of an
// existing anonymous session scope counts as activity: the session marker is
// refreshed, so a read-heavy session keeps its 30-day TTL.
func TestV113ReadRefreshesEphemeralMarker(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	app.scopeSweepAt.Store(time.Now().UnixNano())

	tokenBytes, err := os.ReadFile(app.config.Get().TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(app.handler())
	t.Cleanup(web.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, app, web, strings.TrimSpace(string(tokenBytes)), "/mcp")
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"key": "session-note", "value": "SESSION"}})
	if err != nil {
		t.Fatalf("remember call failed: %v", err)
	}
	if result.IsError {
		t.Fatalf("remember failed: %s", toolText(result))
	}

	scopes := filepath.Join(app.config.Get().StateDir, "scopes")
	entries, err := os.ReadDir(scopes)
	if err != nil {
		t.Fatalf("scope directory unreadable: %v", err)
	}
	marked := ""
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(scopes, entry.Name())
		if _, err := os.Stat(filepath.Join(directory, ephemeralSessionMarker)); err == nil {
			marked = directory
		}
	}
	if marked == "" {
		t.Fatal("no ephemeral session scope was created")
	}
	marker := filepath.Join(marked, ephemeralSessionMarker)
	old := time.Now().Add(-ephemeralSessionTTL - 24*time.Hour)
	for _, path := range []string{marker, filepath.Join(marked, "notes.json"), marked} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	notes, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "recall", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("recall call failed: %v", err)
	}
	if notes.IsError || !strings.Contains(toolText(notes), "SESSION") {
		t.Fatalf("recall failed: %v %s", notes.IsError, toolText(notes))
	}
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("ephemeral marker unreadable: %v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("read did not refresh the ephemeral marker: mtime %v", info.ModTime())
	}
	if err := app.cleanupStaleScopes(context.Background(), app.config.Get().StateDir, ephemeralSessionTTL, 0); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if _, err := os.Stat(marked); err != nil {
		t.Fatalf("read-refreshed session scope was removed: %v", err)
	}
}

// TestV113CleanupStaleScopes covers the 30 day cleanup of anonymous MCP
// session state.
func TestV113CleanupStaleScopes(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	stateDir := app.config.Get().StateDir
	scopesDir := filepath.Join(stateDir, "scopes")

	// Durable scopes: an explicit scope and a ChatGPT conversation.
	explicitPath, err := app.scopeStatePath(nil, "durable-explicit", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	chatGPTRequest := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"openai/session": "conv-durable"}}}
	chatGPTPath, err := app.scopeStatePath(chatGPTRequest, "", "notes.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{explicitPath, chatGPTPath} {
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// An expired anonymous session scope carries the ephemeral marker.
	expired := filepath.Join(scopesDir, "expired-session")
	if err := os.MkdirAll(expired, 0700); err != nil {
		t.Fatal(err)
	}
	expiredMarker := filepath.Join(expired, ephemeralSessionMarker)
	if err := os.WriteFile(expiredMarker, []byte("ephemeral\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expiredNotes := filepath.Join(expired, "notes.json")
	if err := os.WriteFile(expiredNotes, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A fresh anonymous session scope stays untouched.
	fresh := filepath.Join(scopesDir, "fresh-session")
	if err := os.MkdirAll(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, ephemeralSessionMarker), []byte("ephemeral\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "notes.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// An empty scope directory holds no state at all.
	orphan := filepath.Join(scopesDir, "empty-scope")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-ephemeralSessionTTL - 24*time.Hour)
	for _, path := range []string{expiredNotes, expiredMarker, expired} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{explicitPath, chatGPTPath} {
		directory := filepath.Dir(path)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(directory, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.cleanupStaleScopes(context.Background(), stateDir, ephemeralSessionTTL, 0); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired anonymous session scope survived: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("empty orphan scope directory survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh anonymous session scope was removed: %v", err)
	}
	for _, path := range []string{explicitPath, chatGPTPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("durable scope state was removed: %v", err)
		}
	}

	// A missing state directory is not an error.
	if err := app.cleanupStaleScopes(context.Background(), filepath.Join(stateDir, "missing"), ephemeralSessionTTL, 0); err != nil {
		t.Fatalf("cleanup of a missing state directory failed: %v", err)
	}
}

// TestV113ScopeKeyEphemeralFlag pins the namespace rules of the scope key.
func TestV113ScopeKeyEphemeralFlag(t *testing.T) {
	key, ephemeral, err := resolveScopeKey(nil, "explicit-scope")
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral {
		t.Fatal("an explicit scope must not be ephemeral")
	}
	if !strings.HasPrefix(key, "amg-scope-v1:") {
		t.Fatalf("explicit scope key = %q, want the amg-scope-v1 namespace", key)
	}
	chatGPTRequest := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: mcp.Meta{"openai/session": "conv-1"}}}
	key, ephemeral, err = resolveScopeKey(chatGPTRequest, "")
	if err != nil {
		t.Fatal(err)
	}
	if ephemeral {
		t.Fatal("a ChatGPT conversation scope must not be ephemeral")
	}
	if !strings.HasPrefix(key, "amg-chatgpt-scope-v1:") {
		t.Fatalf("ChatGPT scope key = %q, want the amg-chatgpt-scope-v1 namespace", key)
	}
	if _, _, err := resolveScopeKey(nil, ""); !errors.Is(err, errTaskScopeRequired) {
		t.Fatalf("missing scope error = %v, want errTaskScopeRequired", err)
	}
}
