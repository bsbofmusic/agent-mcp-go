package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeTreeFixture writes one file below root, creating parent directories.
func writeTreeFixture(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIgnoreRejectsFIFOSources(t *testing.T) {
	for _, source := range []string{".gitignore", ".git/info/exclude"} {
		for _, writerConnected := range []bool{false, true} {
			name := source + "/no-writer"
			if writerConnected {
				name = source + "/writer-connected"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, filepath.FromSlash(source))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
				if writerConnected {
					writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer writer.Close()
					if _, err := writer.WriteString("secret.txt\n"); err != nil {
						t.Fatal(err)
					}
				}
				// Bound the old blocking open/read in a child process. Cancellation
				// kills and reaps it, closing every reader FD before TempDir cleanup;
				// the parent's connected writer is closed on every result path.
				// Allow the race runtime's one-second exit delay as well as
				// process startup, while keeping blocking regressions bounded.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIgnoreFIFOProbe$", "-test.count=1")
				cmd.Env = append(os.Environ(), "AMG_IGNORE_FIFO_ROOT="+root, "AMG_IGNORE_FIFO_SOURCE="+source)
				out, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("ignore source %s blocked on FIFO (writer connected=%v): %v", source, writerConnected, ctx.Err())
				}
				if err != nil {
					t.Fatalf("FIFO probe: %v %s", err, out)
				}
			})
		}
	}
}

func TestIgnoreFIFOProbe(t *testing.T) {
	root := os.Getenv("AMG_IGNORE_FIFO_ROOT")
	if root == "" {
		return
	}
	workspace, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	matcher := NewIgnoreMatcher(root, IgnoreOptions{})
	matcher.workspace = workspace
	source := filepath.Join(root, filepath.FromSlash(os.Getenv("AMG_IGNORE_FIFO_SOURCE")))
	if rules := matcher.readIgnoreFile(source); len(rules) != 0 {
		t.Fatalf("non-regular ignore source produced %d rules", len(rules))
	}
}

func TestIgnoreDefaultExcludesHideDirectoryAndItsContent(t *testing.T) {
	root := t.TempDir()
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	for name := range defaultIgnoredNames {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Join(dir, "deep"), 0700); err != nil {
			t.Fatal(err)
		}
		if !ig.IsIgnored(dir, true) {
			t.Fatalf("default exclude %q not applied to the directory", name)
		}
		if !ig.IsIgnored(filepath.Join(dir, "deep", "file.js"), false) {
			t.Fatalf("default exclude %q not applied to content below it", name)
		}
	}
	keep := writeTreeFixture(t, root, "src/index.ts", "x")
	if ig.IsIgnored(keep, false) {
		t.Fatal("ordinary project file must not be ignored")
	}
	if ig.IsIgnored(root, true) {
		t.Fatal("the work root itself is never ignored")
	}
}

func TestIgnoreReadsWorkRootGitignore(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "secret.txt\nlogs/\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig.IsIgnored(filepath.Join(root, "secret.txt"), false) {
		t.Fatal("gitignore pattern for a file was not applied")
	}
	if !ig.IsIgnored(filepath.Join(root, "logs"), true) {
		t.Fatal("gitignore directory pattern was not applied")
	}
	if !ig.IsIgnored(filepath.Join(root, "logs", "today.log"), false) {
		t.Fatal("content below a gitignored directory must stay ignored")
	}
	if ig.IsIgnored(filepath.Join(root, "keep.txt"), false) {
		t.Fatal("unrelated file must stay visible")
	}
	// A pattern without a slash matches at any depth.
	writeTreeFixture(t, root, "deep/nested/secret.txt", "x")
	if !ig.IsIgnored(filepath.Join(root, "deep", "nested", "secret.txt"), false) {
		t.Fatal("slashless gitignore pattern must match at any depth")
	}
}

func TestIgnoreReadsGitInfoExclude(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".git/info/exclude", "local-only.tmp\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig.IsIgnored(filepath.Join(root, "local-only.tmp"), false) {
		t.Fatal(".git/info/exclude pattern was not applied")
	}
	if ig.IsIgnored(filepath.Join(root, "other.tmp"), false) {
		t.Fatal("unrelated file must stay visible")
	}
}

func TestIgnoreNestedGitignoreOverridesParent(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "*.log\n")
	writeTreeFixture(t, root, "sub/.gitignore", "!keep.log\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if ig.IsIgnored(filepath.Join(root, "sub", "keep.log"), false) {
		t.Fatal("a nested negation must override the parent .gitignore")
	}
	if !ig.IsIgnored(filepath.Join(root, "sub", "other.log"), false) {
		t.Fatal("the parent .gitignore must still apply inside the nested directory")
	}
	if !ig.IsIgnored(filepath.Join(root, "top.log"), false) {
		t.Fatal("the parent .gitignore must still apply at the top level")
	}
}

func TestIgnoreNegationCannotReincludeBelowExcludedDirectory(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "build/\n!build/keep.txt\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig.IsIgnored(filepath.Join(root, "build", "keep.txt"), false) {
		t.Fatal("a file below an excluded directory cannot be re-included")
	}
}

func TestIgnoreLastMatchingPatternWinsInsideOneFile(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "*.log\n!keep.log\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if ig.IsIgnored(filepath.Join(root, "keep.log"), false) {
		t.Fatal("the later negation must win inside one file")
	}
	if !ig.IsIgnored(filepath.Join(root, "other.log"), false) {
		t.Fatal("the earlier pattern must still apply")
	}

	root2 := t.TempDir()
	writeTreeFixture(t, root2, ".gitignore", "!keep.log\n*.log\n")
	ig2 := NewIgnoreMatcher(root2, IgnoreOptions{})
	if !ig2.IsIgnored(filepath.Join(root2, "keep.log"), false) {
		t.Fatal("the later pattern must win inside one file")
	}
}

func TestIgnoreAnchoredPatternOnlyMatchesBelowItsOwnDirectory(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "/artefacts/\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig.IsIgnored(filepath.Join(root, "artefacts"), true) {
		t.Fatal("an anchored pattern must match at the .gitignore directory")
	}
	if ig.IsIgnored(filepath.Join(root, "sub", "artefacts"), true) {
		t.Fatal("an anchored pattern must not match at another depth")
	}
}

func TestIgnoreOutsideWorkRootIsNeverIgnored(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTreeFixture(t, outside, "secret.txt", "x")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if ig.IsIgnored(filepath.Join(outside, "secret.txt"), false) {
		t.Fatal("paths outside the work root must never be matched")
	}
}

func TestIgnoreShouldPruneAlwaysPrunesNodeModulesAndGit(t *testing.T) {
	root := t.TempDir()
	ig := NewIgnoreMatcher(root, IgnoreOptions{DisableDefaultPatterns: true})
	if !ig.ShouldPrune("node_modules", filepath.Join(root, "node_modules")) {
		t.Fatal("node_modules must always be pruned")
	}
	if !ig.ShouldPrune(".git", filepath.Join(root, ".git")) {
		t.Fatal(".git must always be pruned")
	}
	if ig.ShouldPrune("dist", filepath.Join(root, "dist")) {
		t.Fatal("dist must not be pruned when the default patterns are disabled")
	}
	writeTreeFixture(t, root, ".gitignore", "cache/\n")
	ig2 := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig2.ShouldPrune("cache", filepath.Join(root, "cache")) {
		t.Fatal("a gitignored directory must be pruned")
	}
}

func TestIgnoreSkipsInvalidPatternButKeepsValidOnes(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "[unclosed\nok.txt\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	if !ig.IsIgnored(filepath.Join(root, "ok.txt"), false) {
		t.Fatal("valid patterns must still apply when another line is invalid")
	}
}

func TestIgnoreExplicitSelectedRootStaysDetectable(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, "node_modules/pkg/index.js", "x")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	target := filepath.Join(root, "node_modules")
	if !ig.IsIgnored(target, true) {
		t.Fatal("an ignored selected root must be reported as ignored")
	}
	// The list_directory escape hatch needs the opposite answer for the
	// children of an explicitly selected ignored root.
	if !ig.IsIgnored(filepath.Join(target, "pkg"), true) {
		t.Fatal("content below an ignored root stays ignored")
	}
}

func TestIgnoreHelperSupportsTreeAndListWiring(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, "src/a.ts", "x")
	writeTreeFixture(t, root, "node_modules/pkg/index.ts", "x")
	writeTreeFixture(t, root, "build/out.js", "x")
	writeTreeFixture(t, root, "keep.txt", "x")
	writeTreeFixture(t, root, ".gitignore", "build/\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})

	// tree wiring: keep an entry unless the policy hides it.
	kept := map[string]bool{}
	for _, name := range []string{"src", "node_modules", "build", "keep.txt"} {
		abs := filepath.Join(root, name)
		isDir := name != "keep.txt"
		if isDir {
			kept[name] = !ig.ShouldPrune(name, abs)
		} else {
			kept[name] = !ig.IsIgnored(abs, false)
		}
	}
	if !kept["src"] || !kept["keep.txt"] {
		t.Fatalf("tree wiring dropped a visible entry: %v", kept)
	}
	if kept["node_modules"] || kept["build"] {
		t.Fatalf("tree wiring kept an ignored entry: %v", kept)
	}

	// list_directory wiring: an explicitly selected ignored root still lists
	// its contents, everything else is filtered.
	target := filepath.Join(root, "node_modules")
	targetIgnored := ig.IsIgnored(target, true)
	if !targetIgnored {
		t.Fatal("the escape hatch needs the selected root to be reported as ignored")
	}
	child := filepath.Join(target, "pkg")
	if !targetIgnored && ig.IsIgnored(child, true) {
		t.Fatal("a visible root must still hide its ignored children")
	}
}

func TestIgnoreUnicodeAndCharacterClasses(t *testing.T) {
	root := t.TempDir()
	writeTreeFixture(t, root, ".gitignore", "资料/*.txt\n报告[中英].log\nfile[!x].txt\n")
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	for _, name := range []string{"资料/中文.txt", "报告中.log", "报告英.log", "filea.txt"} {
		if !ig.IsIgnored(filepath.Join(root, name), false) {
			t.Errorf("Unicode/class pattern missed %q", name)
		}
	}
	for _, name := range []string{"资料/中文.md", "报告日.log", "filex.txt"} {
		if ig.IsIgnored(filepath.Join(root, name), false) {
			t.Errorf("Unicode/class pattern unexpectedly matched %q", name)
		}
	}
}

func TestIgnoreExpandedGitOracle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}
	// Ignore the caller's global excludes and quoting settings; all Git writes
	// are confined to this fixture's native TempDir.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	root := t.TempDir()
	cmd := exec.Command("git", "init", "-q", ".")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git init: %v %s", err, out)
	}
	writeTreeFixture(t, root, ".gitignore", strings.Join([]string{
		"# comment", "*.log", "!keep.log", "blocked/", "!blocked/keep.txt",
		"/root-only.txt", "a/**/b/c.txt", "**/generated?.go", "**/cache/**",
		"cache/**", "!cache/keep.txt", `space\ `, "trimmed ", `\#literal`,
		`\!literal`, `literal\*.txt`, "file[!x].txt", "报告[中英].log",
		"资料/*.txt", "{a,b}.txt", `\[brackets\].txt`, "!shared.tmp", "from-root.local",
	}, "\n")+"\n")
	writeTreeFixture(t, root, "sub/.gitignore", "!keep.log\n*.tmp\n!allow.tmp\n/root-only.txt\n")
	writeTreeFixture(t, root, "sub/deeper/.gitignore", "*.log\n!again.log\n")
	writeTreeFixture(t, root, "blocked/.gitignore", "!keep.txt\n")
	writeTreeFixture(t, root, ".git/info/exclude", "shared.tmp\n*.local\n!from-root.local\n")
	paths := []string{
		"app.log", "keep.log", "sub/keep.log", "sub/drop.log", "sub/deeper/keep.log",
		"sub/deeper/again.log", "sub/scratch.tmp", "sub/allow.tmp", "blocked/keep.txt",
		"root-only.txt", "other/root-only.txt", "sub/root-only.txt", "sub/deeper/root-only.txt",
		"a/b/c.txt", "a/b/b/c.txt", "a/x/y/b/c.txt", "a/x/b/other.txt",
		"generated1.go", "deep/generated2.go", "deep/generated22.go", "deep/cache/data.txt",
		"cache/drop.txt", "cache/keep.txt", "cache/deep/keep.txt", "space ", "space",
		"trimmed", "trimmed ", "#literal", "!literal", "literal*.txt", "literalx.txt",
		"filea.txt", "filex.txt", "报告中.log", "报告英.log", "报告日.txt", "资料/中文.txt",
		"资料/中文.md", "{a,b}.txt", "a.txt", "[brackets].txt", "shared.tmp",
		"other.local", "from-root.local", "visible.txt",
	}
	for _, name := range paths {
		writeTreeFixture(t, root, name, "fixture")
	}
	cmd = exec.Command("git", "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
			t.Fatalf("git check-ignore oracle failed: %v", err)
		}
	}
	ignored := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		ignored[name] = true
	}
	matcher := NewIgnoreMatcher(root, IgnoreOptions{DisableDefaultPatterns: true})
	for _, name := range paths {
		if got := matcher.IsIgnored(filepath.Join(root, name), false); got != ignored[name] {
			t.Errorf("%q: matcher=%v git=%v", name, got, ignored[name])
		}
	}
	t.Logf("Git oracle compared %d paths", len(paths))
}

func TestIgnoreRejectsSymlinkedSourceOutsideRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeTreeFixture(t, outside, ".gitignore", "visible.txt\n")
	if err := os.Symlink(filepath.Join(outside, ".gitignore"), filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	matcher := NewIgnoreMatcher(root, IgnoreOptions{})
	for _, name := range []string{"visible.txt", "sub/visible.txt"} {
		if matcher.IsIgnored(filepath.Join(root, name), false) {
			t.Errorf("matcher consumed out-of-root ignore source for %q", name)
		}
	}
}

func TestIgnoreMatchesGitCheckIgnoreOracle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available on this host")
	}
	root := t.TempDir()
	init := exec.Command("git", "init", "-q", ".")
	init.Dir = root
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v %s", err, out)
	}
	writeTreeFixture(t, root, ".gitignore", "*.log\nbuild/\n!build/keep.txt\nsecret.txt\nshared.tmp\n")
	writeTreeFixture(t, root, "sub/.gitignore", "!keep.log\n*.tmp\n")
	writeTreeFixture(t, root, ".git/info/exclude", "!shared.tmp\n")
	writeTreeFixture(t, root, "app.log", "x")
	writeTreeFixture(t, root, "sub/keep.log", "x")
	writeTreeFixture(t, root, "sub/other.log", "x")
	writeTreeFixture(t, root, "sub/scratch.tmp", "x")
	writeTreeFixture(t, root, "build/keep.txt", "x")
	writeTreeFixture(t, root, "secret.txt", "x")
	writeTreeFixture(t, root, "shared.tmp", "x")
	writeTreeFixture(t, root, "keep.txt", "x")

	paths := []string{
		"app.log", "sub/keep.log", "sub/other.log", "sub/scratch.tmp",
		"build/keep.txt", "secret.txt", "shared.tmp", "keep.txt",
	}
	cmd := exec.Command("git", "check-ignore", "--no-index", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, _ := cmd.Output()
	gitIgnored := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			gitIgnored[line] = true
		}
	}
	ig := NewIgnoreMatcher(root, IgnoreOptions{})
	for _, p := range paths {
		got := ig.IsIgnored(filepath.Join(root, filepath.FromSlash(p)), false)
		if got != gitIgnored[p] {
			t.Errorf("path %q: go ignored=%v, git check-ignore ignored=%v", p, got, gitIgnored[p])
		}
	}
}
