package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestImportAndSnapshotExport(t *testing.T) {
	a, _, _ := testApp(t)
	c := a.config.Get()
	src := filepath.Join(c.WorkRoot, "source.txt")
	if err := os.WriteFile(src, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := a.exportFile(context.Background(), ExportInput{Path: src})
	if err != nil {
		t.Fatal(err)
	}
	if v.Bytes != 3 || v.Sha256 == "" || v.ExpiresInMs == 0 {
		t.Fatalf("invalid export %#v", v)
	}
	if err = os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	// Read the snapshot behind the ticket; it must not change when the original changes.
	key := filepath.Base(v.URL)
	item, ok := a.transfers.get(key)
	if !ok {
		t.Fatal("snapshot ticket absent")
	}
	b, err := os.ReadFile(item.path)
	if err != nil || string(b) != "old" {
		t.Fatalf("snapshot changed: %q", b)
	}
	a.transfers.forget(key)
	staged := filepath.Join(c.StateDir, "staged")
	if err = os.MkdirAll(staged, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(staged, "one"), []byte("imported"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(c.WorkRoot, "dest.txt")
	imp, err := a.importFile(context.Background(), ImportInput{File: "staged:one", Path: dest})
	if err != nil || imp.Bytes != 8 {
		t.Fatalf("import failed %v %#v", err, imp)
	}
	if _, err = a.importFile(context.Background(), ImportInput{File: "staged:one", Path: dest}); err == nil {
		t.Fatal("staged file reused")
	}
	if _, err = a.importFile(context.Background(), ImportInput{File: "staged:../../etc/shadow", Path: dest}); err == nil {
		t.Fatal("import path traversal accepted")
	}
}

func TestImageToolOverMCP(t *testing.T) {
	a, _, token := testApp(t)
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.config.Get().WorkRoot, "test.png")
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	// Registry-level functionality: execute the image tool using the public SDK.
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-mcp-go-test", Version: "1"}, nil)
	a.registerFileParity(server)
	client := mcp.NewClient(&mcp.Implementation{Name: "image-test", Version: "1"}, nil)
	ct, st := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), st)
	session, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": path}})
	if err != nil || result.IsError {
		t.Fatalf("image tool error %v", err)
	}
	found := false
	for _, v := range result.Content {
		if c, ok := v.(*mcp.ImageContent); ok && c.MIMEType == "image/png" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing image/png MCP content block")
	}
	_ = token
}

func TestMCPExportResourceSnapshot(t *testing.T) {
	a, _, _ := testApp(t)
	p := filepath.Join(a.config.Get().WorkRoot, "source.txt")
	if e := os.WriteFile(p, []byte("immutable"), 0600); e != nil {
		t.Fatal(e)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "resource-test", Version: "1"}, nil)
	a.registerFileParity(server)
	a.registerExportResources(server)
	t1, t2 := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), t1)
	client := mcp.NewClient(&mcp.Implementation{Name: "resource-client", Version: "1"}, nil)
	session, e := client.Connect(context.Background(), t2, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	res, e := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "export_host_file", Arguments: map[string]any{"path": p}})
	if e != nil || res.IsError {
		t.Fatalf("export failed: %v", e)
	}
	var uri string
	for _, v := range res.Content {
		if link, ok := v.(*mcp.ResourceLink); ok {
			uri = link.URI
		}
	}
	if uri == "" {
		t.Fatal("missing native MCP resource_link")
	}
	if e = os.WriteFile(p, []byte("changed"), 0600); e != nil {
		t.Fatal(e)
	}
	read, e := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if e != nil {
		t.Fatal(e)
	}
	if len(read.Contents) != 1 || string(read.Contents[0].Blob) != "immutable" {
		t.Fatal("MCP resource did not preserve immutable snapshot")
	}
}

func TestOrphanExportCleanupIsScoped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "exports")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	orphan := filepath.Join(dir, "snapshot-abcd")
	unrelated := filepath.Join(dir, "important.txt")
	if e := os.WriteFile(orphan, []byte("old"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(unrelated, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := cleanupOrphanExports(root); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(orphan); !os.IsNotExist(e) {
		t.Fatal("orphan snapshot was retained")
	}
	if _, e := os.Stat(unrelated); e != nil {
		t.Fatal("unrelated file was deleted")
	}
}
