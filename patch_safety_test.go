package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func patchAssertNoStaging(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".agent-mcp-patch-") {
			t.Errorf("staging file remains: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPatchRejectsEscapingParentSymlinks(t *testing.T) {
	for _, operation := range []string{"add", "update", "delete", "move"} {
		t.Run(operation, func(t *testing.T) {
			a, _, _ := testApp(t)
			root, outside := a.config.Get().WorkRoot, t.TempDir()
			victim := filepath.Join(outside, "victim.txt")
			if err := os.WriteFile(victim, []byte("outside\n"), 0640); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
				t.Fatal(err)
			}
			patchWriteFile(t, a, "source.txt", "source\n")
			body := map[string]string{
				"add":    "*** Add File: escape/new.txt\n+bad\n",
				"update": "*** Update File: escape/victim.txt\n@@\n-outside\n+bad\n",
				"delete": "*** Delete File: escape/victim.txt\n",
				"move":   "*** Update File: source.txt\n*** Move to: escape/new.txt\n@@\n-source\n+bad\n",
			}[operation]
			if err := patchRun(t, a, patchBegin+"\n"+body+patchEnd); err == nil {
				t.Error("patch accepted an escaping parent symlink")
			}
			if got := patchReadFile(t, victim); got != "outside\n" {
				t.Errorf("outside file changed: %q", got)
			}
			if _, err := os.Lstat(filepath.Join(outside, "new.txt")); !os.IsNotExist(err) {
				t.Errorf("outside destination created: %v", err)
			}
			patchAssertNoStaging(t, root)
			patchAssertNoStaging(t, outside)
		})
	}
}

func TestPatchParentSwitchAfterStagingStaysInRoot(t *testing.T) {
	a, _, _ := testApp(t)
	root, outside := a.config.Get().WorkRoot, t.TempDir()
	parent, oldParent := filepath.Join(root, "parent"), filepath.Join(root, "old-parent")
	patchWriteFile(t, a, "parent/target.txt", "before\n")
	victim := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(victim, []byte("outside\n"), 0640); err != nil {
		t.Fatal(err)
	}
	real := patchCommitFile
	patchCommitFile = func(ch *stagedChange) error {
		if err := os.Rename(parent, oldParent); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, parent); err != nil {
			t.Fatal(err)
		}
		// A path-based rename would now consume this outside inode.
		if err := os.WriteFile(filepath.Join(outside, ch.temp), []byte("attacker\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return real(ch)
	}
	t.Cleanup(func() { patchCommitFile = real })
	_ = patchRun(t, a, "*** Begin Patch\n*** Update File: parent/target.txt\n@@\n-before\n+after\n*** End Patch")
	if got := patchReadFile(t, victim); got != "outside\n" {
		t.Fatalf("parent switch wrote outside the root: %q", got)
	}
	patchAssertNoStaging(t, oldParent)
}

func TestPatchRollbackPreservesExternalChanges(t *testing.T) {
	for _, operation := range []string{"update", "add", "move", "delete"} {
		for _, change := range []string{"inode", "bytes", "mode"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				a, _, _ := testApp(t)
				root := a.config.Get().WorkRoot
				first := filepath.Join(root, "first.txt")
				if operation != "add" {
					patchWriteFile(t, a, "first.txt", "original\n")
				}
				body := map[string]string{
					"update": "*** Update File: first.txt\n@@\n-original\n+committed\n",
					"add":    "*** Add File: first.txt\n+committed\n",
					"move":   "*** Update File: first.txt\n*** Move to: moved.txt\n@@\n-original\n+committed\n",
					"delete": "*** Delete File: first.txt\n",
				}[operation]
				external := first
				if operation == "move" {
					external = filepath.Join(root, "moved.txt")
				}
				var observed os.FileInfo
				var want string
				real := patchCommitFile
				patchCommitFile = func(ch *stagedChange) error {
					destination := ch.moveTo
					if strings.HasSuffix(destination, "second.txt") {
						want = "committed\n"
						if operation == "delete" {
							want = "external\n"
						}
						switch change {
						case "inode":
							tmp := filepath.Join(root, "external-replacement")
							if err := os.WriteFile(tmp, []byte(want), 0600); err != nil {
								t.Fatal(err)
							}
							if operation != "delete" {
								info, err := os.Stat(external)
								if err != nil {
									t.Fatal(err)
								}
								if err := os.Chmod(tmp, info.Mode()); err != nil {
									t.Fatal(err)
								}
							}
							if err := os.Rename(tmp, external); err != nil {
								t.Fatal(err)
							}
						case "bytes":
							want = "external\n"
							if err := os.WriteFile(external, []byte(want), 0600); err != nil {
								t.Fatal(err)
							}
						case "mode":
							if operation == "delete" {
								if err := os.WriteFile(external, []byte(want), 0600); err != nil {
									t.Fatal(err)
								}
							}
							if err := os.Chmod(external, 0400); err != nil {
								t.Fatal(err)
							}
						}
						var err error
						observed, err = os.Stat(external)
						if err != nil {
							t.Fatal(err)
						}
						return errors.New("injected second commit failure")
					}
					return real(ch)
				}
				t.Cleanup(func() { patchCommitFile = real })
				err := patchRun(t, a, patchBegin+"\n"+body+"*** Add File: second.txt\n+second\n"+patchEnd)
				if err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
					t.Errorf("missing rollback conflict: %v", err)
				}
				got, readErr := os.ReadFile(external)
				if readErr != nil || string(got) != want {
					t.Errorf("rollback changed external content: %q, %v", got, readErr)
				}
				info, statErr := os.Stat(external)
				if statErr != nil || !os.SameFile(info, observed) || info.Mode() != observed.Mode() {
					t.Errorf("rollback replaced external inode or mode: %v", statErr)
				}
				patchAssertNoStaging(t, root)
			})
		}
	}
}

func TestPatchOutputAndWorkingSetCaps(t *testing.T) {
	t.Run("output exceeds configured cap", func(t *testing.T) {
		a, _, _ := testApp(t)
		a.config.mu.Lock()
		a.config.cfg.MaxFileBytes = 1 << 20
		a.config.mu.Unlock()
		before := strings.Repeat("x", (1<<20)-1) + "\n"
		path := patchWriteFile(t, a, "cap.txt", before)
		if err := patchRun(t, a, "*** Begin Patch\n*** Update File: cap.txt\n@@\n+x\n*** End Patch"); err == nil {
			t.Error("oversized after content accepted")
		}
		if got := patchReadFile(t, path); got != before {
			t.Error("cap failure changed the file")
		}
	})
	t.Run("cumulative bytes", func(t *testing.T) {
		a, _, _ := testApp(t)
		// Small bounded fixture; input + before + after exceeds 16 MiB.
		before := strings.Repeat("x", 5<<20) + "\n"
		first := patchWriteFile(t, a, "first.txt", before)
		second := patchWriteFile(t, a, "second.txt", before)
		patch := "*** Begin Patch\n*** Update File: first.txt\n@@\n+x\n*** Update File: second.txt\n@@\n+y\n*** End Patch"
		if err := patchRun(t, a, patch); err == nil {
			t.Error("cumulative budget accepted")
		}
		if patchReadFile(t, first) != before || patchReadFile(t, second) != before {
			t.Error("budget failure mutated files")
		}
		patchAssertNoStaging(t, a.config.Get().WorkRoot)
	})
	t.Run("input line cap before parsing", func(t *testing.T) {
		a, _, _ := testApp(t)
		patch := "*** Begin Patch\n*** Add File: large.txt\n" + strings.Repeat("+\n", 100001) + "*** End Patch"
		if err := patchRun(t, a, patch); err == nil {
			t.Error("input line cap accepted")
		}
		if _, err := os.Lstat(filepath.Join(a.config.Get().WorkRoot, "large.txt")); !os.IsNotExist(err) {
			t.Errorf("line cap created output: %v", err)
		}
	})
}

func TestPatchNoClobberAfterPreflight(t *testing.T) {
	for _, operation := range []string{"add", "move"} {
		t.Run(operation, func(t *testing.T) {
			a, _, _ := testApp(t)
			root := a.config.Get().WorkRoot
			patchWriteFile(t, a, "source.txt", "source\n")
			body := "*** Add File: destination.txt\n+patch\n"
			if operation == "move" {
				body = "*** Update File: source.txt\n*** Move to: destination.txt\n@@\n-source\n+patch\n"
			}
			real := patchCommitFile
			patchCommitFile = func(ch *stagedChange) error {
				if err := os.WriteFile(filepath.Join(root, "destination.txt"), []byte("external\n"), 0640); err != nil {
					t.Fatal(err)
				}
				return real(ch)
			}
			t.Cleanup(func() { patchCommitFile = real })
			if err := patchRun(t, a, patchBegin+"\n"+body+patchEnd); err == nil {
				t.Error("concurrent destination creation was overwritten")
			}
			if got := patchReadFile(t, filepath.Join(root, "destination.txt")); got != "external\n" {
				t.Errorf("external destination changed: %q", got)
			}
			if got := patchReadFile(t, filepath.Join(root, "source.txt")); got != "source\n" {
				t.Errorf("source changed: %q", got)
			}
			patchAssertNoStaging(t, root)
		})
	}
}

func TestPatchMoveRollbackPreservesRecreatedSource(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	first := patchWriteFile(t, a, "first.txt", "original\n")
	real := patchCommitFile
	patchCommitFile = func(ch *stagedChange) error {
		if ch.moveTo == "second.txt" {
			if err := os.WriteFile(first, []byte("external\n"), 0640); err != nil {
				t.Fatal(err)
			}
			return errors.New("injected second commit failure")
		}
		return real(ch)
	}
	t.Cleanup(func() { patchCommitFile = real })
	patch := "*** Begin Patch\n*** Update File: first.txt\n*** Move to: moved.txt\n@@\n-original\n+committed\n*** Add File: second.txt\n+second\n*** End Patch"
	if err := patchRun(t, a, patch); err == nil || !strings.Contains(err.Error(), "rollback incomplete") {
		t.Errorf("source recreation conflict not reported: %v", err)
	}
	if got := patchReadFile(t, first); got != "external\n" {
		t.Errorf("recreated source overwritten: %q", got)
	}
	patchAssertNoStaging(t, root)
}

func TestPatchPartialMoveRemovalFailuresAreReported(t *testing.T) {
	for _, failCleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup succeeds", true: "cleanup fails"}[failCleanup], func(t *testing.T) {
			a, _, _ := testApp(t)
			root := a.config.Get().WorkRoot
			first := patchWriteFile(t, a, "first.txt", "original\n")
			real := patchRemoveFile
			patchRemoveFile = func(ref *patchFileRef) error {
				if ref.name == "first.txt" {
					return errors.New("injected source removal failure")
				}
				if ref.name == "moved.txt" && failCleanup {
					return errors.New("injected destination cleanup failure")
				}
				return real(ref)
			}
			t.Cleanup(func() { patchRemoveFile = real })
			patch := "*** Begin Patch\n*** Update File: first.txt\n*** Move to: moved.txt\n@@\n-original\n+committed\n*** End Patch"
			err := patchRun(t, a, patch)
			if err == nil || !strings.Contains(err.Error(), "source removal failure") {
				t.Fatalf("missing source error: %v", err)
			}
			if failCleanup {
				if !strings.Contains(err.Error(), "rollback incomplete") || !strings.Contains(err.Error(), "destination cleanup failure") {
					t.Errorf("cleanup failure swallowed: %v", err)
				}
			} else if _, err := os.Lstat(filepath.Join(root, "moved.txt")); !os.IsNotExist(err) {
				t.Errorf("partial Move destination remains: %v", err)
			}
			if got := patchReadFile(t, first); got != "original\n" {
				t.Errorf("source changed: %q", got)
			}
			patchAssertNoStaging(t, root)
		})
	}
}

func TestPatchMatchingWorkLimitsPreflightWithoutMutation(t *testing.T) {
	for _, kind := range []string{"comparisons", "bytes", "shared across files"} {
		t.Run(kind, func(t *testing.T) {
			a, _, _ := testApp(t)
			work := newPatchMatchBudget(context.Background())
			before := "a\n"
			body := "*** Update File: first.txt\n@@\n-a\n+A\n"
			switch kind {
			case "comparisons":
				// This controlled worst-prefix mismatch needs only hundreds of
				// comparisons on the old implementation, never billions.
				before = strings.Repeat("a\n", 32)
				body = "*** Update File: first.txt\n@@\n" + strings.Repeat("-a\n", 15) + "-b\n+B\n"
				work.comparisons = 8
			case "bytes":
				before = strings.Repeat("a", 128) + "\n"
				body = "*** Update File: first.txt\n@@\n-" + strings.TrimSuffix(before, "\n") + "\n+A\n"
				work.bytes = 16
			case "shared across files":
				body += "*** Update File: second.txt\n@@\n-a\n+A\n"
				work.comparisons = 1
			}
			first := patchWriteFile(t, a, "first.txt", before)
			second := patchWriteFile(t, a, "second.txt", "a\n")
			_, err := a.applyCodexPatchWithMatchBudget(context.Background(), patchBegin+"\n"+body+patchEnd, work)
			if !errors.Is(err, errPatchMatchBudget) {
				t.Errorf("missing matching budget error: %v", err)
			}
			if patchReadFile(t, first) != before || patchReadFile(t, second) != "a\n" {
				t.Error("matching budget failure changed a target")
			}
			patchAssertNoStaging(t, a.config.Get().WorkRoot)
		})
	}
}

// Err closes Done after a deterministic number of checks; this models a
// cancellation during the match loop without scheduling or wall-clock races.
type patchCancelAfterChecks struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *patchCancelAfterChecks) Err() error {
	c.checks--
	if c.checks <= 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestPatchMatchingChecksCancellationDuringSearch(t *testing.T) {
	a, _, _ := testApp(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &patchCancelAfterChecks{Context: base, cancel: cancel, checks: 2}
	work := newPatchMatchBudget(ctx)
	before := strings.Repeat("a\n", 128)
	first := patchWriteFile(t, a, "first.txt", before)
	body := "*** Update File: first.txt\n@@\n" + strings.Repeat("-a\n", 63) + "-b\n+B\n"
	// The injected budget context cancels after matching has begun. The app
	// context stays live so entry/preflight checks cannot satisfy this test.
	_, err := a.applyCodexPatchWithMatchBudget(context.Background(), patchBegin+"\n"+body+patchEnd, work)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("matcher did not propagate cancellation: %v", err)
	}
	if work.comparisons >= maxPatchComparisons {
		t.Error("fixture did not enter the matching loop")
	}
	if got := patchReadFile(t, first); got != before {
		t.Error("canceled matcher changed the file")
	}
	patchAssertNoStaging(t, a.config.Get().WorkRoot)
}

func TestPatchRechecksSourceAfterPreflight(t *testing.T) {
	for _, operation := range []string{"update", "move", "delete"} {
		for _, change := range []string{"inode", "bytes", "mode"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				a, _, _ := testApp(t)
				root := a.config.Get().WorkRoot
				first := patchWriteFile(t, a, "first.txt", "original\n")
				body := map[string]string{
					"update": "*** Update File: first.txt\n@@\n-original\n+changed\n",
					"move":   "*** Update File: first.txt\n*** Move to: moved.txt\n@@\n-original\n+changed\n",
					"delete": "*** Delete File: first.txt\n",
				}[operation]
				real := patchCommitFile
				var observed os.FileInfo
				want := "original\n"
				patchCommitFile = func(ch *stagedChange) error {
					destination := ch.moveTo
					if strings.HasSuffix(destination, "trigger.txt") {
						switch change {
						case "inode":
							tmp := filepath.Join(root, "external-replacement")
							if err := os.WriteFile(tmp, []byte(want), 0644); err != nil {
								t.Fatal(err)
							}
							if err := os.Rename(tmp, first); err != nil {
								t.Fatal(err)
							}
						case "bytes":
							want = "external\n"
							if err := os.WriteFile(first, []byte(want), 0644); err != nil {
								t.Fatal(err)
							}
						case "mode":
							if err := os.Chmod(first, 0400); err != nil {
								t.Fatal(err)
							}
						}
						var err error
						observed, err = os.Stat(first)
						if err != nil {
							t.Fatal(err)
						}
					}
					return real(ch)
				}
				t.Cleanup(func() { patchCommitFile = real })
				err := patchRun(t, a, patchBegin+"\n*** Add File: trigger.txt\n+trigger\n"+body+patchEnd)
				if err == nil {
					t.Error("source changed after preflight was accepted")
				}
				info, statErr := os.Stat(first)
				if statErr != nil || !os.SameFile(info, observed) || info.Mode() != observed.Mode() {
					t.Errorf("changed source inode or mode was lost: %v", statErr)
				}
				if got, err := os.ReadFile(first); err != nil || string(got) != want {
					t.Errorf("changed source content was lost: %q, %v", got, err)
				}
				if _, err := os.Lstat(filepath.Join(root, "moved.txt")); !os.IsNotExist(err) {
					t.Errorf("move committed despite source conflict: %v", err)
				}
				patchAssertNoStaging(t, root)
			})
		}
	}
}
