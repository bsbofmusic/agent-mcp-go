package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNativeFilesystemToolsRejectIntermediateSymlinkEscape(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "sub", "victim.txt")
	if err := os.WriteFile(victim, []byte("outside-sentinel\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "root-boundary", Version: "1"}, nil)
	app.registerCore(server)
	app.registerParity(server)
	client := contractClient(t, server)
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "alias/sub/victim.txt"}},
		{"write_file", map[string]any{"path": "alias/sub/new.txt", "content": "escape"}},
		{"list_directory", map[string]any{"path": "alias/sub"}},
		{"tree", map[string]any{"path": "alias/sub"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("expected bounded tool error: %v %+v", err, result)
			}
		})
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "outside-sentinel\n" {
		t.Fatalf("outside changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "sub", "new.txt")); !os.IsNotExist(err) {
		t.Fatal("write escaped WorkRoot")
	}
}

func TestAtomicWorkspaceWritePreservesModeAndCreatesParents(t *testing.T) {
	app, _, _ := testApp(t)
	path := filepath.Join(app.config.Get().WorkRoot, "nested", "more", "file.txt")
	if err := atomicWorkspaceFile(app.config.Get(), path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if err := atomicWorkspaceFile(app.config.Get(), path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("mode lost: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("bad replacement: %q %v", data, err)
	}
}
