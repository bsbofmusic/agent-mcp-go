package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexPatchFileOpsAndPreflight(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	initial := filepath.Join(root, "main.txt")
	if e := os.WriteFile(initial, []byte("alpha\nbeta\ngamma\n"), 0640); e != nil {
		t.Fatal(e)
	}
	patch := "*** Begin Patch\n*** Update File: main.txt\n@@\n alpha\n-beta\n+BETTER\n gamma\n*** Add File: notes.txt\n+hello\n*** End Patch"
	result, e := a.applyCodexPatch(context.Background(), patch)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(result.Output, "update") {
		t.Fatal("missing patch summary")
	}
	after, e := os.ReadFile(initial)
	if e != nil || string(after) != "alpha\nBETTER\ngamma\n" {
		t.Fatalf("wrong updated content %q err %v", after, e)
	}
	note, e := os.ReadFile(filepath.Join(root, "notes.txt"))
	if e != nil || string(note) != "hello\n" {
		t.Fatal("add file failed")
	}
	if _, e := a.applyCodexPatch(context.Background(), patch); e == nil {
		t.Fatal("duplicate add reapply accepted")
	}
	again, _ := os.ReadFile(initial)
	if string(again) != string(after) {
		t.Fatal("failed patch partially overwrote file")
	}
	move := "*** Begin Patch\n*** Update File: main.txt\n*** Move to: moved.txt\n@@\n-BETTER\n+IMPROVED\n*** End Patch"
	_, e = a.applyCodexPatch(context.Background(), move)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(initial); !os.IsNotExist(e) {
		t.Fatal("old source not removed after move")
	}
	moved, e := os.ReadFile(filepath.Join(root, "moved.txt"))
	if e != nil || string(moved) != "alpha\nIMPROVED\ngamma\n" {
		t.Fatalf("move content wrong %q %v", moved, e)
	}
	_, e = a.applyCodexPatch(context.Background(), "*** Begin Patch\n*** Delete File: notes.txt\n*** End Patch")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(e) {
		t.Fatal("delete failed")
	}
	if _, e = a.applyCodexPatch(context.Background(), "*** Begin Patch\n*** Add File: ../forbidden\n+fail\n*** End Patch"); e == nil {
		t.Fatal("patch escaped work root")
	}
}

// failCommitFor makes the commit of the named destination fail so the
// best-effort rollback path can be exercised deterministically.
func failCommitFor(name string) func() {
	real := patchCommitFile
	patchCommitFile = func(ch *stagedChange) error {
		if strings.HasSuffix(ch.moveTo, name) {
			return errors.New("injected commit failure")
		}
		return real(ch)
	}
	return func() { patchCommitFile = real }
}

func TestCodexPatchRollsBackCommittedFilesWhenCommitFails(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	first := filepath.Join(root, "first.txt")
	second := filepath.Join(root, "second.txt")
	if e := os.WriteFile(first, []byte("original\n"), 0640); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(second, []byte("target\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(failCommitFor("second.txt"))

	patch := "*** Begin Patch\n*** Update File: first.txt\n@@\n-original\n+updated\n*** Update File: second.txt\n@@\n-target\n+changed\n*** End Patch"
	_, e := a.applyCodexPatch(context.Background(), patch)
	if e == nil {
		t.Fatal("patch reported success although a commit failed")
	}
	if !strings.Contains(e.Error(), "injected commit failure") {
		t.Fatalf("error lost the underlying cause: %v", e)
	}
	after, _ := os.ReadFile(first)
	if string(after) != "original\n" {
		t.Fatalf("rollback did not restore the first file: %q", after)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("rollback changed the file mode: %v", info.Mode().Perm())
	}
	failed, _ := os.ReadFile(second)
	if string(failed) != "target\n" {
		t.Fatalf("the file whose commit failed was modified: %q", failed)
	}
}

func TestCodexPatchRollbackRemovesFilesCreatedBeforeTheFailure(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	t.Cleanup(failCommitFor("second.txt"))

	patch := "*** Begin Patch\n*** Add File: first.txt\n+one\n*** Add File: second.txt\n+two\n*** End Patch"
	if _, e := a.applyCodexPatch(context.Background(), patch); e == nil {
		t.Fatal("patch reported success although a commit failed")
	}
	if _, e := os.Stat(filepath.Join(root, "first.txt")); !os.IsNotExist(e) {
		t.Fatal("rollback did not remove the file created before the failure")
	}
}

func TestCodexPatchRefusesToClobberExistingFiles(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	existing := filepath.Join(root, "existing.txt")
	moved := filepath.Join(root, "moved.txt")
	if e := os.WriteFile(existing, []byte("keep\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(moved, []byte("other\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := a.applyCodexPatch(context.Background(), "*** Begin Patch\n*** Add File: existing.txt\n+clobber\n*** End Patch"); e == nil {
		t.Fatal("Add overwrote an existing file")
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep\n" {
		t.Fatalf("refused Add still changed the file: %q", b)
	}
	move := "*** Begin Patch\n*** Update File: existing.txt\n*** Move to: moved.txt\n@@\n-keep\n+changed\n*** End Patch"
	if _, e := a.applyCodexPatch(context.Background(), move); e == nil {
		t.Fatal("Move overwrote an existing destination")
	}
	if b, _ := os.ReadFile(moved); string(b) != "other\n" {
		t.Fatalf("refused Move still changed the destination: %q", b)
	}
	if b, _ := os.ReadFile(existing); string(b) != "keep\n" {
		t.Fatalf("refused Move still changed the source: %q", b)
	}
}

func TestCodexPatchPreservesFileModeOnUpdate(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	p := filepath.Join(root, "mode.txt")
	if e := os.WriteFile(p, []byte("a\n"), 0640); e != nil {
		t.Fatal(e)
	}
	if _, e := a.applyCodexPatch(context.Background(), "*** Begin Patch\n*** Update File: mode.txt\n@@\n-a\n+b\n*** End Patch"); e != nil {
		t.Fatal(e)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("update changed the file mode: %v", info.Mode().Perm())
	}
}

func TestCodexPatchRejectsDuplicateTargets(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	p := filepath.Join(root, "dup.txt")
	if e := os.WriteFile(p, []byte("a\n"), 0644); e != nil {
		t.Fatal(e)
	}
	patch := "*** Begin Patch\n*** Update File: dup.txt\n@@\n-a\n+b\n*** Update File: dup.txt\n@@\n-b\n+c\n*** End Patch"
	if _, e := a.applyCodexPatch(context.Background(), patch); e == nil {
		t.Fatal("patch with two operations on the same file was accepted")
	}
	if b, _ := os.ReadFile(p); string(b) != "a\n" {
		t.Fatalf("rejected patch mutated the file: %q", b)
	}
}

func TestCodexPatchRefusesSymlinkTargets(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	real := filepath.Join(root, "real.txt")
	if e := os.WriteFile(real, []byte("a\n"), 0644); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(root, "link.txt")
	if e := os.Symlink(real, link); e != nil {
		t.Skipf("symlinks unsupported here: %v", e)
	}
	patch := "*** Begin Patch\n*** Update File: link.txt\n@@\n-a\n+b\n*** End Patch"
	if _, e := a.applyCodexPatch(context.Background(), patch); e == nil {
		t.Fatal("patch followed a symlink target")
	}
	if b, _ := os.ReadFile(real); string(b) != "a\n" {
		t.Fatalf("the symlink target was modified: %q", b)
	}
}
