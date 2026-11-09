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

func TestGitStatusReportsCleanDirtyAndErrors(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	repo := filepath.Join(root, "repo")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	git("init", "-q", repo)
	server := mcp.NewServer(&mcp.Implementation{Name: "git-status-test", Version: "1"}, nil)
	app.registerCore(server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go server.Run(ctx, serverTransport)
	client := mcp.NewClient(&mcp.Implementation{Name: "git-status-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(dir string) (*mcp.CallToolResult, string) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "git_status", Arguments: map[string]any{"directory": dir}})
		if err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, content := range res.Content {
			if c, ok := content.(*mcp.TextContent); ok {
				text.WriteString(c.Text)
			}
		}
		return res, text.String()
	}
	t.Run("clean unborn repository is explicit", func(t *testing.T) {
		res, text := call(repo)
		if res.IsError || !strings.Contains(text, "nothing to commit") {
			t.Fatalf("clean repository must report success explicitly: isError=%v text=%q", res.IsError, text)
		}
	})
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("staged\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("-C", repo, "add", "tracked.txt")
	if err := os.WriteFile(tracked, []byte("modified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("staged unstaged and untracked changes remain visible", func(t *testing.T) {
		res, text := call(repo)
		for _, want := range []string{"new file:", "modified:", "tracked.txt", "untracked.txt"} {
			if res.IsError || !strings.Contains(text, want) {
				t.Errorf("dirty status missing %q: isError=%v text=%q", want, res.IsError, text)
			}
		}
		if strings.Contains(text, "working tree clean") || strings.Contains(text, "nothing to commit") {
			t.Errorf("dirty repository reported clean: %q", text)
		}
	})
	for name, dir := range map[string]string{"not a repository": root, "missing directory": filepath.Join(root, "missing")} {
		t.Run(name, func(t *testing.T) {
			res, text := call(dir)
			if !res.IsError || strings.TrimSpace(text) == "" || strings.Contains(text, "working tree clean") {
				t.Fatalf("Git failure must remain an explicit error: isError=%v text=%q", res.IsError, text)
			}
		})
	}
}
