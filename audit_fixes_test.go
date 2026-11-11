package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestCanonicalOrigin pins the RFC 6454 canonicalization contract: origins
// are case-insensitive, while malformed values and "null" never canonicalize.
func TestCanonicalOrigin(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"https://claude.ai", "https://claude.ai", true},
		{"HTTPS://Claude.AI", "https://claude.ai", true},
		{"https://CLAUDE.ai", "https://claude.ai", true},
		{"app://Obsidian.MD", "app://obsidian.md", true},
		{"APP://Obsidian.MD", "app://obsidian.md", true},
		{"tauri://localhost", "tauri://localhost", true},
		{"vscode-webview://abcdef", "vscode-webview://abcdef", true},
		{"https://claude.ai:8443", "https://claude.ai:8443", true},
		{"http://127.0.0.1:3100", "http://127.0.0.1:3100", true},
		{"null", "", false},
		{"", "", false},
		{"https://claude.ai/", "", false},
		{"https://claude.ai/path", "", false},
		{"https://claude.ai?query=1", "", false},
		{"https://claude.ai#fragment", "", false},
		{"https://user:secret@claude.ai", "", false},
		{" https://claude.ai", "", false},
		{"https://claude.ai ", "", false},
		{"https://claude .ai", "", false},
		{"https://claude.ai\n", "", false},
		{"javascript:alert(1)", "", false},
		{"mailto:agent@example.com", "", false},
		{"claude.ai", "", false},
		{"://claude.ai", "", false},
	}
	for _, tc := range cases {
		got, ok := canonicalOrigin(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("canonicalOrigin(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}

// TestAllowsOriginCanonicalMatching proves that a configured origin matches
// case-insensitively, that malformed values fail closed, and that "*" stays an
// explicit escape hatch.
func TestAllowsOriginCanonicalMatching(t *testing.T) {
	cfg := Config{
		PublicURL:      "https://mcp.example.com/",
		AllowedOrigins: []string{"https://claude.ai", "app://obsidian.md"},
	}
	for _, origin := range []string{
		"https://claude.ai",
		"HTTPS://Claude.AI",
		"https://CLAUDE.ai",
		"APP://Obsidian.MD",
		"app://Obsidian.MD",
		"HTTPS://MCP.Example.COM",
	} {
		if !cfg.allowsOrigin(origin) {
			t.Errorf("allowsOrigin(%q) = false, want true", origin)
		}
	}
	for _, origin := range []string{
		"null",
		"https://evil.invalid",
		"https://claude.ai.evil.invalid",
		"https://claude.ai/path",
		"https://claude.ai?x=1",
		"https://user@claude.ai",
		"javascript:alert(1)",
	} {
		if cfg.allowsOrigin(origin) {
			t.Errorf("allowsOrigin(%q) = true, want false", origin)
		}
	}
	if !cfg.allowsOrigin("") {
		t.Error("an empty origin must stay allowed for non-browser clients")
	}
	wildcard := Config{PublicURL: "https://mcp.example.com", AllowedOrigins: []string{"*"}}
	for _, origin := range []string{"null", "javascript:alert(1)", "https://anything.invalid"} {
		if !wildcard.allowsOrigin(origin) {
			t.Errorf("wildcard allowsOrigin(%q) = false, want true", origin)
		}
	}
}

// TestNormalizeAllowedOriginsCanonical proves the config loader stores the
// canonical form and rejects anything that is not a bare origin.
func TestNormalizeAllowedOriginsCanonical(t *testing.T) {
	got, err := normalizeAllowedOrigins([]string{" HTTPS://Claude.AI ", "APP://Obsidian.MD", "*", "tauri://LOCALHOST"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	want := []string{"https://claude.ai", "app://obsidian.md", "*", "tauri://localhost"}
	if len(got) != len(want) {
		t.Fatalf("normalize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalize = %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"null", "https://claude.ai/path", "https://user:pw@claude.ai", "javascript:alert(1)", "claude.ai"} {
		if _, err := normalizeAllowedOrigins([]string{bad}); err == nil {
			t.Errorf("normalizeAllowedOrigins(%q) accepted a malformed origin", bad)
		}
	}
	if out, err := normalizeAllowedOrigins(nil); err != nil || out != nil {
		t.Errorf("empty allowlist = (%v, %v), want (nil, nil)", out, err)
	}
}

// TestOriginHeaderMultiValueDenied proves that a request carrying more than
// one Origin header is rejected instead of letting an upstream choose one.
func TestOriginHeaderMultiValueDenied(t *testing.T) {
	_, server, token := v113OriginServer(t, func(cfg *Config) {
		cfg.AllowedOrigins = []string{"https://claude.ai"}
	})
	post := func(origins []string, bearer string) (int, string, http.Header) {
		t.Helper()
		request, err := http.NewRequest("POST", server.URL+"/mcp", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		for _, origin := range origins {
			request.Header.Add("Origin", origin)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body := make([]byte, 512)
		n, _ := response.Body.Read(body)
		return response.StatusCode, string(body[:n]), response.Header
	}
	if status, body, _ := post([]string{"https://claude.ai", "https://evil.invalid"}, token); status != http.StatusForbidden || !strings.Contains(body, "origin denied") {
		t.Fatalf("duplicate Origin: status=%d body=%q, want 403 origin denied", status, body)
	}
	if status, body, _ := post([]string{"https://claude.ai", "https://claude.ai"}, token); status != http.StatusForbidden {
		t.Fatalf("duplicate allowed Origin: status=%d body=%q, want 403", status, body)
	}
	// Bearer authentication stays in front of the origin check.
	if status, body, header := post([]string{"https://claude.ai", "https://evil.invalid"}, ""); status != http.StatusUnauthorized || !strings.HasPrefix(header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("duplicate Origin without token: status=%d body=%q challenge=%q, want 401 with a Bearer challenge", status, body, header.Get("WWW-Authenticate"))
	}
	// A single canonical-equivalent origin is still accepted.
	if status, body, _ := post([]string{"HTTPS://Claude.AI"}, token); status == http.StatusForbidden || strings.Contains(body, "origin denied") {
		t.Fatalf("single uppercase Origin refused: status=%d body=%q", status, body)
	}
}

// TestVerifyDirWithinRootRejectsSymlinkEscape proves the kernel-level
// confinement: a symlink inside work_root that points outside of it is not a
// usable directory, even though its lexical path is inside.
func TestVerifyDirWithinRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "project")
	if err := os.MkdirAll(inside, 0700); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := verifyDirWithinRoot(root, inside); err != nil {
		t.Fatalf("a real subdirectory was rejected: %v", err)
	}
	if err := verifyDirWithinRoot(root, root); err != nil {
		t.Fatalf("work_root itself was rejected: %v", err)
	}
	if err := verifyDirWithinRoot(root, escape); err == nil {
		t.Fatal("a symlink escaping work_root was accepted")
	}
	if err := verifyDirWithinRoot(root, outside); err == nil {
		t.Fatal("a lexically external directory was accepted")
	}
	file := filepath.Join(root, "regular-file")
	if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirWithinRoot(root, file); err == nil {
		t.Fatal("a regular file was accepted as a working directory")
	}
}

// TestSymlinkEscapeRejectedByExecPaths proves that every process-starting
// path refuses a symlinked working directory that leaves work_root.
func TestSymlinkEscapeRejectedByExecPaths(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := app.parityRunCommand(ctx, RunCommandInput{Command: "git", Args: []string{"status"}, Workdir: escape}); err == nil {
		t.Fatal("run_command accepted a symlinked workdir outside work_root")
	}
	if _, err := app.gitOp(ctx, escape, "status"); err == nil {
		t.Fatal("gitOp accepted a symlinked directory outside work_root")
	}
	if _, err := app.gitWithEnv(ctx, escape, nil, "status"); err == nil {
		t.Fatal("gitWithEnv accepted a symlinked directory outside work_root")
	}
	if _, err := app.sessions.startForOwner(app.config.Get(), CommandInput{Cmd: "pwd", Workdir: escape}, ""); err == nil {
		t.Fatal("exec_command accepted a symlinked workdir outside work_root")
	}

	// The configuration loader rejects a symlinked default_workdir too.
	cfg := baseTestConfig(t, root, escape)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, cfg)
	if _, err := readConfig(cfgPath); err == nil {
		t.Fatal("readConfig accepted a symlinked default_workdir outside work_root")
	}
}

// TestSymlinkEscapeRejectedByFileTools proves the built-in file tools keep
// refusing a symlink that leaves work_root.
func TestSymlinkEscapeRejectedByFileTools(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	server := mcp.NewServer(&mcp.Implementation{Name: "symlink-audit", Version: "1"}, nil)
	app.registerCore(server)
	app.registerParity(server)
	c, sv := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(context.Background(), sv) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "symlink-audit-client", Version: "1"}, nil).Connect(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	for _, name := range []string{"list_directory", "read_file"} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{"path": escape}})
		if err != nil {
			t.Fatalf("%s transport: %v", name, err)
		}
		if !result.IsError {
			t.Fatalf("%s accepted a symlink escaping work_root: %s", name, toolText(result))
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "run_command", Arguments: map[string]any{
		"command": "git", "args": []string{"status"}, "workdir": escape,
	}})
	if err != nil {
		t.Fatalf("run_command transport: %v", err)
	}
	if !result.IsError {
		t.Fatalf("run_command accepted a symlinked workdir: %s", toolText(result))
	}
}

// TestScopeLockRaceWithConcurrentSweeps is the regression test of the TOCTOU
// fix: notes, plans, handover snapshots and the ephemeral marker of brand-new
// durable and ephemeral scopes are written while two cleanup sweeps run in a
// tight loop (one with the live 10 minute grace, one reclaiming every expired
// ephemeral scope). The test must observe zero ENOENT errors and zero data
// races under -race.
func TestScopeLockRaceWithConcurrentSweeps(t *testing.T) {
	root := t.TempDir()
	app := newTestAppWithConfig(t, baseTestConfig(t, root, root))
	// Disable the automatic hourly sweep: this test drives sweeps directly.
	app.scopeSweepAt.Store(time.Now().UnixNano())
	stateDir := app.config.Get().StateDir

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Anonymous MCP session scopes only exist for a real stateful transport,
	// so drive them over the HTTP handler the daemon actually serves.
	tokenBytes, err := os.ReadFile(app.config.Get().TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(app.handler())
	t.Cleanup(web.Close)
	const ephemeralWorkers = 4
	sessions := make([]*mcp.ClientSession, 0, ephemeralWorkers)
	for i := 0; i < ephemeralWorkers; i++ {
		sessions = append(sessions, handoverHTTPClient(t, ctx, app, web, strings.TrimSpace(string(tokenBytes)), "/mcp"))
	}

	failures := make(chan string, 8192)
	record := func(format string, args ...any) {
		select {
		case failures <- fmt.Sprintf(format, args...):
		default:
		}
	}

	stop := make(chan struct{})
	var sweepers sync.WaitGroup
	sweep := func(maxAge, grace time.Duration) {
		defer sweepers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := app.cleanupStaleScopes(context.Background(), stateDir, maxAge, grace); err != nil {
				record("cleanup(maxAge=%v,grace=%v): %v", maxAge, grace, err)
				return
			}
		}
	}
	sweepers.Add(2)
	go sweep(ephemeralSessionTTL, 10*time.Minute) // live traffic sweep
	go sweep(0, 0)                                // expired scope + empty dir sweep

	var workers sync.WaitGroup
	const durableWorkers = 4
	const rounds = 20
	for i := 0; i < durableWorkers; i++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			scope := fmt.Sprintf("audit-durable-%02d", worker)
			for round := 0; round < rounds; round++ {
				value := fmt.Sprintf("%s-v%d", scope, round)
				if _, err := app.rememberNote(ctx, nil, NoteInput{Scope: scope, Key: fmt.Sprintf("k%d", round), Value: value}); err != nil {
					record("durable remember %s: %v", scope, err)
					return
				}
				if _, err := app.recallNotes(ctx, nil, RecallInput{Scope: scope}); err != nil {
					record("durable recall %s: %v", scope, err)
					return
				}
				if _, err := app.parityUpdatePlan(ctx, nil, PlanInput{Scope: scope, Plan: []PlanStep{{Step: value, Status: "in_progress"}}}); err != nil {
					record("durable plan %s: %v", scope, err)
					return
				}
				brief, err := app.taskHandover(ctx, nil, scope)
				if err != nil {
					record("durable handover %s: %v", scope, err)
					return
				}
				if !strings.Contains(brief, value) {
					record("durable handover %s lost %q", scope, value)
					return
				}
			}
		}(i)
	}
	for i := 0; i < ephemeralWorkers; i++ {
		workers.Add(1)
		go func(worker int, session *mcp.ClientSession) {
			defer workers.Done()
			for round := 0; round < rounds; round++ {
				value := fmt.Sprintf("ephemeral-%d-v%d", worker, round)
				call := func(name string, args map[string]any) (*mcp.CallToolResult, error) {
					return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				}
				remember, err := call("remember", map[string]any{"key": fmt.Sprintf("k%d", round), "value": value})
				if err != nil {
					record("ephemeral remember transport: %v", err)
					return
				}
				if remember.IsError {
					record("ephemeral remember: %s", toolText(remember))
					return
				}
				recall, err := call("recall", map[string]any{})
				if err != nil {
					record("ephemeral recall transport: %v", err)
					return
				}
				if recall.IsError {
					record("ephemeral recall: %s", toolText(recall))
					return
				}
				plan, err := call("update_plan", map[string]any{"plan": []map[string]string{{"step": value, "status": "pending"}}})
				if err != nil {
					record("ephemeral plan transport: %v", err)
					return
				}
				if plan.IsError {
					record("ephemeral plan: %s", toolText(plan))
					return
				}
				brief, err := call("get_agent_brief", map[string]any{})
				if err != nil {
					record("ephemeral brief transport: %v", err)
					return
				}
				if brief.IsError {
					record("ephemeral brief: %s", toolText(brief))
					return
				}
			}
		}(i, sessions[i])
	}
	workers.Wait()
	close(stop)
	sweepers.Wait()
	close(failures)

	failed := false
	for message := range failures {
		failed = true
		if strings.Contains(message, "no such file or directory") || strings.Contains(message, "ENOENT") {
			t.Errorf("ENOENT under the scope lock: %s", message)
			continue
		}
		t.Errorf("%s", message)
	}
	if failed {
		t.Fatal("concurrent scope state work failed; see the errors above")
	}

	// Durable scopes must survive both sweeps with their data intact.
	for i := 0; i < durableWorkers; i++ {
		scope := fmt.Sprintf("audit-durable-%02d", i)
		last := fmt.Sprintf("%s-v%d", scope, rounds-1)
		notes, err := app.recallNotes(context.Background(), nil, RecallInput{Scope: scope})
		if err != nil {
			t.Fatalf("durable recall %s: %v", scope, err)
		}
		if !strings.Contains(notes.Output, last) {
			t.Fatalf("durable scope %s lost its final note: %s", scope, notes.Output)
		}
		brief, err := app.taskHandover(context.Background(), nil, scope)
		if err != nil {
			t.Fatalf("durable handover %s: %v", scope, err)
		}
		if !strings.Contains(brief, last) {
			t.Fatalf("durable scope %s lost its handover state", scope)
		}
	}

	// The sweeps must not have broken ephemeral scopes either: a fresh write
	// after the sweeps stop recreates the marked directory and keeps it.
	if result, err := sessions[0].CallTool(context.Background(), &mcp.CallToolParams{Name: "remember", Arguments: map[string]any{"key": "after-sweep", "value": "ALIVE"}}); err != nil || result.IsError {
		t.Fatalf("ephemeral write after the sweeps failed: %v %s", err, toolText(result))
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, scopeSubdirName))
	if err != nil {
		t.Fatalf("scope directory unreadable: %v", err)
	}
	marked := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(stateDir, scopeSubdirName, entry.Name(), ephemeralSessionMarker)); err == nil {
			marked++
		}
	}
	if marked == 0 {
		t.Fatal("no ephemeral session scope survived the concurrent sweeps")
	}

	// The per-scope lock pool keeps no idle entries after the run.
	app.stateGuards.mu.Lock()
	idle := len(app.stateGuards.locks)
	app.stateGuards.mu.Unlock()
	if idle != 0 {
		t.Fatalf("scope lock pool retained %d idle locks", idle)
	}
}
