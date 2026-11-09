package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewIncrementalProjectScopeAndCard(t *testing.T) {
	app, _, _ := testApp(t)
	ctx := context.Background()
	repo := filepath.Join(app.config.Get().WorkRoot, "project")
	if err := os.MkdirAll(filepath.Join(repo, "module"), 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "--initial-branch=main")
	write("module/probe.txt", "baseline\n")
	write("sibling.txt", "sibling baseline\n")
	git("add", "-A")
	git("-c", "user.name=AMG Test", "-c", "user.email=amg@example.invalid", "commit", "-qm", "initial")
	write("module/probe.txt", "baseline\nreview-visible\n")
	write("module/new.txt", "new untracked file\n")
	write("sibling.txt", "sibling must not appear\n")
	server := mcp.NewServer(&mcp.Implementation{Name: "review-test", Version: "1"}, nil)
	app.registerParity(server)
	app.registerReviewUI(server)
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(ctx, st)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "review-client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		args["directory"], args["scope"] = filepath.Join(repo, "module"), "review-test"
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "show_changes", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("review failed: %v %s", err, toolText(result))
		}
		return result
	}
	before := git("count-objects", "-v")
	readonly := call(map[string]any{})
	if !strings.Contains(toolText(readonly), "2 files changed") || git("count-objects", "-v") != before || git("for-each-ref", "refs/amg/review") != "" {
		t.Fatal("generic review stored snapshots without requesting advancement")
	}
	first := call(map[string]any{"advance": true})
	if body := toolText(first); !strings.Contains(body, "review-visible") || !strings.Contains(body, "new untracked file") ||
		strings.Contains(body, "sibling must not appear") || strings.Contains(body, "a/module/") || first.Meta[reviewMetaKey] == nil {
		t.Fatalf("wrong scoped review or missing card metadata: %s", body)
	}
	if diff := git("diff", "--cached", "--name-only"); diff != "" {
		t.Fatalf("review altered the real Git index: %s", diff)
	}
	if out := toolText(call(map[string]any{})); !strings.Contains(out, "0 files changed") {
		t.Fatalf("unchanged worktree still shows an incremental diff: %s", out)
	}
	out := toolText(call(map[string]any{"since": "project_open", "advance": false, "include_patch": false}))
	if !strings.Contains(out, "2 files changed") || strings.Contains(out, "review-visible") {
		t.Fatalf("project baseline or patch preference incorrect: %s", out)
	}
	card, err := client.ReadResource(ctx, &mcp.ReadResourceParams{URI: reviewUIURI})
	if err != nil || len(card.Contents) != 1 || card.Contents[0].MIMEType != reviewMIME ||
		!strings.Contains(card.Contents[0].Text, "ui/notifications/tool-result") {
		t.Fatalf("MCP Apps resource unavailable: %v", err)
	}
}
