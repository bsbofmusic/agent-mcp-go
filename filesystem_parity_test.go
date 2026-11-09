package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCodexFreeDirectoryAndTreeFormatting(t *testing.T) {
	app, _, _ := testApp(t)
	root := filepath.Join(app.config.Get().WorkRoot, "fixture")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git", ".codegraph", "node_modules", "assets"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.md"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), make([]byte, 1536), 0600); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "filesystem-fixture", Version: "1"}, nil)
	app.registerCore(server)
	app.registerParity(server)
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), st)
	client := mcp.NewClient(&mcp.Implementation{Name: "filesystem-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %v", name, err, result)
		}
		text, _ := result.StructuredContent.(map[string]any)["output"].(string)
		return text
	}
	listing := call("list_directory", map[string]any{"path": root})
	for _, expected := range []string{"dir\t-\t.codegraph/", "dir\t-\tassets/", "file\t1.5KB\ta.bin", "file\t5B\treadme.md"} {
		if !strings.Contains(listing, expected) {
			t.Fatalf("missing %q in directory listing: %q", expected, listing)
		}
	}
	if strings.Contains(listing, ".git") || strings.Contains(listing, "node_modules") {
		t.Fatalf("default ignored entries leaked: %q", listing)
	}
	tree := call("tree", map[string]any{"path": root, "depth": 1})
	if !strings.HasPrefix(tree, ".\n├── .codegraph/\n├── assets/\n├── a.bin\n└── readme.md") {
		t.Fatalf("directory-first CodexFree tree format differs: %q", tree)
	}
}
