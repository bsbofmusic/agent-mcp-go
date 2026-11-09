package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkspaceRootedReadHelpers(t *testing.T) {
	root := t.TempDir()
	c := Config{WorkRoot: root}
	path := writeTreeFixture(t, root, "dir/中文.txt", "inside")
	writeTreeFixture(t, root, "dir/z.txt", "z")
	writeTreeFixture(t, root, "dir/a.txt", "a")
	file, err := openWorkspaceFile(c, path)
	if err != nil {
		t.Fatal(err)
	}
	// The helper has already closed its Root; this independent FD remains valid.
	data, err := io.ReadAll(file)
	file.Close()
	if err != nil || string(data) != "inside" {
		t.Fatalf("root lifetime invalidated file: %q %v", data, err)
	}
	entries, err := readWorkspaceDirectory(c, filepath.Join(root, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !reflect.DeepEqual(names, []string{"a.txt", "z.txt", "中文.txt"}) {
		t.Fatalf("directory order changed: %v", names)
	}
	info, err := statWorkspacePath(c, path)
	if err != nil || info.Size() != int64(len("inside")) {
		t.Fatalf("stat failed: %v %v", info, err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	file, err = openWorkspaceFile(c, filepath.Join(root, "alias", "中文.txt"))
	if err != nil {
		t.Fatalf("safe relative symlink should work: %v", err)
	}
	file.Close()
}

func TestWorkspaceRejectsLexicalAndSymlinkEscapes(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	c := Config{WorkRoot: root}
	secret := writeTreeFixture(t, outside, "sub/secret.txt", "OUTSIDE_SENTINEL")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "file-link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{secret, filepath.Join(root, "..", filepath.Base(outside), "sub", "secret.txt"), filepath.Join(root, "link", "sub", "secret.txt"), filepath.Join(root, "file-link"), "relative.txt"} {
		if file, err := openWorkspaceFile(c, path); err == nil {
			file.Close()
			t.Errorf("open accepted escaped path %q", path)
		}
		if _, err := statWorkspacePath(c, path); err == nil {
			t.Errorf("stat accepted escaped path %q", path)
		}
	}
	for _, path := range []string{outside, filepath.Join(root, "link", "sub"), "relative"} {
		if entries, err := readWorkspaceDirectory(c, path); err == nil {
			t.Errorf("ReadDir accepted escaped path %q: %v", path, entries)
		}
	}
}

func TestWorkspaceRejectsIntermediateReplacementAfterStat(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	c := Config{WorkRoot: root}
	path := writeTreeFixture(t, root, "safe/sub/data.txt", "inside")
	writeTreeFixture(t, outside, "sub/data.txt", "OUTSIDE_SENTINEL")
	if _, err := statWorkspacePath(c, path); err != nil {
		t.Fatal(err)
	}
	opened, err := openWorkspaceFile(c, path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	// Both absolute paths are explicitly under this test's own TempDir.
	from, to := filepath.Join(root, "safe"), filepath.Join(root, "held")
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, from); err != nil {
		t.Fatal(err)
	}
	if file, err := openWorkspaceFile(c, path); err == nil {
		file.Close()
		t.Fatal("open followed the replacement out of WorkRoot")
	}
	if _, err := readWorkspaceDirectory(c, filepath.Dir(path)); err == nil {
		t.Fatal("ReadDir followed the intermediate replacement")
	}
	if _, err := statWorkspacePath(c, path); err == nil {
		t.Fatal("Stat followed the intermediate replacement")
	}
	data, err := io.ReadAll(opened)
	if err != nil || string(data) != "inside" {
		t.Fatalf("already opened descriptor changed target: %q %v", data, err)
	}
}
