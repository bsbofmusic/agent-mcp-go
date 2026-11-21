package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeGitForReview puts a PATH-first git wrapper in front of the real binary.
// It records every full-snapshot attempt (`add -A`) and can stall it, so the
// timeout path runs in a second instead of needing a multi-gigabyte repository.
// A stalled wrapper records its own PID and the PID of its sleep, so tests can
// prove the deadline kill left no process behind.
func fakeGitForReview(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "add-attempts.log")
	pidPath := fakeGitPidPath(dir)
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
*"add -A"*)
  printf '%%s\n' "$*" >> '%s'
  if [ "$AMG_FAKE_GIT_STALL" = "1" ]; then
    printf '%%s\n' "$$" >> '%s'
    sleep 30 &
    child=$!
    printf '%%s\n' "$child" >> '%s'
    wait "$child"
  fi
  ;;
esac
exec '%s' "$@"
`, logPath, pidPath, pidPath, realGit)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("PATH", previous)
		_ = os.Unsetenv("AMG_FAKE_GIT_STALL")
	})
	return logPath
}

// fakeGitPidPath is the sibling file where a stalled fake git records the PIDs
// of its wrapper shell and of its sleep child.
func fakeGitPidPath(fakeGitDir string) string {
	return filepath.Join(fakeGitDir, "stall-pids.log")
}

func reviewGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initReviewRepo(t *testing.T, app *App, name string) string {
	t.Helper()
	repo := filepath.Join(app.config.Get().WorkRoot, name)
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, repo, "init", "-q", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("committed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reviewGit(t, repo, "add", ".")
	reviewGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	return repo
}

func callStatelessTool(t *testing.T, srv *httptest.Server, token, name string, arguments map[string]any) map[string]any {
	t.Helper()
	arguments["scope"] = "baseline-guard"
	params := map[string]any{"name": name, "arguments": arguments}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", srv.URL+"/mcp/stateless", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:3100"
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var message map[string]any
	if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	result, ok := message["result"].(map[string]any)
	if response.StatusCode != 200 || !ok || result["isError"] == true {
		t.Fatalf("%s: %s: %v", name, response.Status, message)
	}
	return result
}

// projectOpenRef returns the internal baseline ref the review lifecycle wrote
// for repo, or "" when no baseline was captured.
func projectOpenRef(t *testing.T, repo string) string {
	t.Helper()
	for _, ref := range strings.Split(reviewGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/amg/review"), "\n") {
		if strings.HasSuffix(ref, "/project-open") {
			return ref
		}
	}
	return ""
}

func countAddAttempts(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(data), "add -A")
}

// A project whose baseline snapshot exceeds review_baseline_timeout_seconds
// must degrade to HEAD^{tree}, return the mutation promptly, and stay cheap on
// the following calls until the cooldown expires.
func TestReviewBaselineTimeoutDegradesWithoutBlocking(t *testing.T) {
	app, srv, token := testApp(t)
	repo := initReviewRepo(t, app, "timeout-project")
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := fakeGitForReview(t)
	if err := os.Setenv("AMG_FAKE_GIT_STALL", "1"); err != nil {
		t.Fatal(err)
	}
	timeout := 1
	if _, err := app.config.Update(SettingsInput{ReviewBaselineTimeoutSeconds: &timeout}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app.requestLogger = log.New(&logs, "", 0)

	started := time.Now()
	callStatelessTool(t, srv, token, "exec_command", map[string]any{"cmd": "true", "workdir": repo})
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("mutation blocked %s on the review baseline; want a bounded wait", elapsed)
	}
	if logged := logs.String(); !strings.Contains(logged, `"reason":"timeout"`) {
		t.Fatalf("review baseline log = %q, want the deadline reported as a timeout", logged)
	}
	if attempts := countAddAttempts(t, logPath); attempts != 1 {
		t.Fatalf("full snapshot attempts = %d, want 1", attempts)
	}
	ref := projectOpenRef(t, repo)
	if ref == "" {
		t.Fatal("timeout did not record a degraded baseline ref")
	}
	if tree, head := reviewGit(t, repo, "rev-parse", ref), reviewGit(t, repo, "rev-parse", "HEAD^{tree}"); tree != head {
		t.Fatalf("degraded baseline tree = %s, want HEAD tree %s", tree, head)
	}

	// Drop the ref to force the cooldown check: the second mutation must take
	// the cheap path without retrying `git add -A`.
	reviewGit(t, repo, "update-ref", "-d", ref)
	started = time.Now()
	callStatelessTool(t, srv, token, "exec_command", map[string]any{"cmd": "true", "workdir": repo})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cooldown mutation blocked %s; want the degraded path", elapsed)
	}
	if attempts := countAddAttempts(t, logPath); attempts != 1 {
		t.Fatalf("cooldown retried the full snapshot: attempts = %d, want 1", attempts)
	}
}

// review_baseline_skip_paths must capture HEAD^{tree} without ever running the
// unbounded snapshot.
func TestReviewBaselineSkipPathsUsesHeadTree(t *testing.T) {
	app, srv, token := testApp(t)
	repo := initReviewRepo(t, app, "skip-project")
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := fakeGitForReview(t)
	skip := []string{repo}
	if _, err := app.config.Update(SettingsInput{ReviewBaselineSkipPaths: &skip}); err != nil {
		t.Fatal(err)
	}
	callStatelessTool(t, srv, token, "write_file", map[string]any{
		"path": filepath.Join(repo, "agent.txt"), "content": "agent\n",
	})
	if attempts := countAddAttempts(t, logPath); attempts != 0 {
		t.Fatalf("skip_paths project still ran the full snapshot %d time(s)", attempts)
	}
	ref := projectOpenRef(t, repo)
	if ref == "" {
		t.Fatal("skip_paths did not record a baseline ref")
	}
	if tree, head := reviewGit(t, repo, "rev-parse", ref), reviewGit(t, repo, "rev-parse", "HEAD^{tree}"); tree != head {
		t.Fatalf("skip baseline tree = %s, want HEAD tree %s", tree, head)
	}
}

// run_command carries its directory in workdir; the review baseline must read
// that field instead of the previous hard-coded empty string.
func TestRunCommandBaselineUsesWorkdir(t *testing.T) {
	app, srv, token := testApp(t)
	repo := initReviewRepo(t, app, "workdir-project")
	callStatelessTool(t, srv, token, "run_command", map[string]any{"command": "true", "workdir": repo})
	if ref := projectOpenRef(t, repo); ref == "" {
		t.Fatal("run_command did not capture a baseline in its workdir")
	}
}

// A show_changes workspace snapshot that exceeds review_baseline_timeout_seconds
// must fail with a bounded, actionable error instead of running under the much
// larger command_timeout_seconds budget. It must not return a partial diff, must
// not write a review ref, and must not leave the stalled git child running.
func TestShowChangesSnapshotTimeoutReturnsBoundedError(t *testing.T) {
	app, _, _ := testApp(t)
	ctx := context.Background()
	repo := initReviewRepo(t, app, "show-changes-timeout-project")
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("user edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := fakeGitForReview(t)
	pidPath := fakeGitPidPath(filepath.Dir(logPath))
	if err := os.Setenv("AMG_FAKE_GIT_STALL", "1"); err != nil {
		t.Fatal(err)
	}
	timeout := 1
	if _, err := app.config.Update(SettingsInput{ReviewBaselineTimeoutSeconds: &timeout}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	app.requestLogger = log.New(&logs, "", 0)

	server := mcp.NewServer(&mcp.Implementation{Name: "review-timeout-test", Version: "1"}, nil)
	app.registerParity(server)
	app.registerReviewUI(server)
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(ctx, st)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "review-timeout-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	started := time.Now()
	result, callErr := client.CallTool(ctx, &mcp.CallToolParams{Name: "show_changes", Arguments: map[string]any{
		"directory": repo, "since": "project_open", "advance": true, "scope": "baseline-guard",
	}})
	elapsed := time.Since(started)
	if elapsed > 5*time.Second {
		t.Fatalf("show_changes blocked %s on the snapshot; want a bounded error", elapsed)
	}
	if callErr == nil && (result == nil || !result.IsError) {
		t.Fatalf("show_changes succeeded despite the stalled snapshot: %+v", result)
	}
	message := ""
	if callErr != nil {
		message = callErr.Error()
	} else {
		message = toolText(result)
	}
	if !strings.Contains(message, "timed out") || !strings.Contains(message, "review_baseline_timeout_seconds") {
		t.Fatalf("show_changes error = %q, want an actionable snapshot timeout", message)
	}
	if attempts := countAddAttempts(t, logPath); attempts != 1 {
		t.Fatalf("workspace snapshot attempts = %d, want 1", attempts)
	}
	if refs := strings.TrimSpace(reviewGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/amg/review")); refs != "" {
		t.Fatalf("timed-out show_changes wrote review refs: %s", refs)
	}
	if logged := logs.String(); !strings.Contains(logged, `"reason":"timeout"`) {
		t.Fatalf("show_changes timeout log = %q, want reason=timeout", logged)
	}
	assertStalledGitExited(t, pidPath)
}

// assertStalledGitExited proves the deadline kill removed both the fake git
// wrapper shell and its sleep child rather than leaving them behind.
func assertStalledGitExited(t *testing.T, pidPath string) {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("stalled snapshot recorded no process ids: %v", err)
	}
	pids := strings.Fields(string(data))
	if len(pids) != 2 {
		t.Fatalf("stalled snapshot recorded pids %q, want the shell and its sleep child", pids)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range pids {
		for {
			if _, err := os.Stat(filepath.Join("/proc", pid)); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("stalled snapshot process %s survived the deadline kill", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
