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

func TestLocalGitCommitAndPushViaMCP(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	repo := filepath.Join(root, "git-work")
	remote := filepath.Join(root, "git-remote.git")
	check := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v failed: %v %s", args, e, out)
		}
	}
	check("init", "-q", repo)
	check("init", "--bare", "-q", remote)
	check("-C", repo, "config", "user.name", "Agent MCP Test")
	check("-C", repo, "config", "user.email", "agent-mcp-test@localhost")
	check("-C", repo, "remote", "add", "origin", remote)
	if e := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	check("-C", repo, "add", "a.txt")
	server := mcp.NewServer(&mcp.Implementation{Name: "git-test", Version: "1"}, nil)
	app.registerCore(server)
	app.registerParity(server)
	c, s := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), s)
	client := mcp.NewClient(&mcp.Implementation{Name: "git-client-test", Version: "1"}, nil)
	session, e := client.Connect(context.Background(), c, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	call := func(name string, args map[string]any) {
		t.Helper()
		res, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if e != nil || res.IsError {
			t.Fatalf("tool %s failed %v %v", name, e, res)
		}
	}
	call("git_commit", map[string]any{"message": "test commit", "directory": repo})
	call("git_log", map[string]any{"count": 1, "oneline": true, "directory": repo})
	call("git_push", map[string]any{"remote": "origin", "branch": "HEAD:refs/heads/main", "directory": repo})
	call("git_status", map[string]any{"directory": repo})
	cmd := exec.Command("git", "--git-dir", remote, "show-ref", "refs/heads/main")
	out, e := cmd.CombinedOutput()
	if e != nil || !strings.Contains(string(out), "refs/heads/main") {
		t.Fatalf("local remote did not receive commit: %v %s", e, out)
	}
}
