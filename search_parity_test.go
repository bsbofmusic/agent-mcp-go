package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
)

func TestGlobMatureMatcherPoC(t *testing.T) {
	// Fixed, bounded fixture: compare the replacement's matching primitive
	// with the production regressions below before replacing the parser.
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"资料/报告[中英].{txt,md}", "资料/报告中.txt", true},
		{"资料/?.txt", "资料/😀.txt", true},
		{`\{a,b\}.txt`, "{a,b}.txt", true},
		{"a/**/b/c.txt", "a/b/b/c.txt", true},
		{"file[!x].txt", "filea.txt", true},
		{"file[!x].txt", "filex.txt", false},
		{"*.go", "sub/a.go", false},
		{"**/*.go", "a.go", true},
	} {
		got, err := doublestar.Match(tc.pattern, tc.path)
		if err != nil || got != tc.want {
			t.Errorf("mature matcher %q / %q = %v, %v", tc.pattern, tc.path, got, err)
		}
	}
}

func TestGlobAndGrepCodexInputs(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	if e := os.MkdirAll(filepath.Join(root, "src", "sub"), 0700); e != nil {
		t.Fatal(e)
	}
	files := map[string]string{
		"src/main.go":         "package p\nfunc Alpha() {}",
		"src/sub/helper.go":   "package p\nfunc alphaHelper() {}\nreturn",
		"src/sub/info.txt":    "ANOTHER LINE",
		"src/sub/ignore.json": "{\"key\":\"alpha\"}",
	}
	for name, content := range files {
		if e := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	found, e := app.globFiles(context.Background(), GlobInput{Pattern: "src/**/*.go"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(found.Output, "src/main.go") || !strings.Contains(found.Output, "src/sub/helper.go") {
		t.Fatalf("recursive glob mismatch %q", found.Output)
	}
	if strings.Contains(found.Output, "info.txt") {
		t.Fatalf("glob included non-Go file: %q", found.Output)
	}
	scoped, e := app.globFiles(context.Background(), GlobInput{Path: "src", Pattern: "**/*.go"})
	if e != nil || !strings.Contains(scoped.Output, "main.go") || strings.Contains(scoped.Output, "src/main.go") {
		t.Fatalf("scoped glob must return paths relative to src: %v %q", e, scoped.Output)
	}
	grep, e := app.grepFiles(context.Background(), GrepInput{
		Path: "src", Pattern: "ALPHA", Include: "**/*.go", IgnoreCase: true, Context: 1, MaxResults: 3,
	})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(grep.Output, "src/main.go:2:") || !strings.Contains(grep.Output, "src/sub/helper.go:2:") {
		t.Fatalf("grep flags ignored: %q", grep.Output)
	}
	if strings.Contains(grep.Output, "ignore.json") {
		t.Fatalf("include glob ignored %q", grep.Output)
	}
	only, e := app.grepFiles(context.Background(), GrepInput{Path: "src", Pattern: "alpha", FilesOnly: true, MaxResults: 1})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(strings.TrimSpace(only.Output), "\n") > 1 {
		t.Fatalf("filesOnly/match cap error: %q", only.Output)
	}
}

func TestSearchGlobHonoursIgnoreRules(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, ".gitignore", "secret.txt\nlogs/\n")
	writeTreeFixture(t, root, "src/main.go", "package p")
	writeTreeFixture(t, root, "secret.txt", "secret")
	writeTreeFixture(t, root, "logs/today.log", "secret")
	writeTreeFixture(t, root, "node_modules/pkg/index.js", "secret")
	writeTreeFixture(t, root, "src/nested/.gitignore", "hidden.go\n")
	writeTreeFixture(t, root, "src/nested/hidden.go", "secret")
	writeTreeFixture(t, root, "src/nested/visible.go", "package p")

	found, e := app.globFiles(context.Background(), GlobInput{Pattern: "**/*.go"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(found.Output, "src/main.go") || !strings.Contains(found.Output, "src/nested/visible.go") {
		t.Fatalf("glob lost visible matches: %q", found.Output)
	}
	for _, hidden := range []string{"secret.txt", "logs/today.log", "node_modules", "hidden.go"} {
		if strings.Contains(found.Output, hidden) {
			t.Fatalf("glob surfaced ignored path %q: %q", hidden, found.Output)
		}
	}

	grep, e := app.grepFiles(context.Background(), GrepInput{Pattern: "secret"})
	if e != nil {
		t.Fatal(e)
	}
	for _, hidden := range []string{"secret.txt:", "logs/today.log:", "node_modules/", "src/nested/hidden.go:"} {
		if strings.Contains(grep.Output, hidden) {
			t.Fatalf("grep surfaced ignored path %q: %q", hidden, grep.Output)
		}
	}
}

func TestSearchGlobSupportsUpstreamGlobSyntax(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	for _, name := range []string{"a.go", "b.md", "c.txt", "d/e.go", "d/f.md", "x.txt"} {
		writeTreeFixture(t, root, name, "x")
	}
	cases := []struct {
		pattern string
		want    []string
		absent  []string
	}{
		{pattern: "*.{go,md}", want: []string{"a.go", "b.md"}, absent: []string{"c.txt", "d/e.go"}},
		{pattern: "[ab].*", want: []string{"a.go", "b.md"}, absent: []string{"c.txt"}},
		{pattern: "[!x]1.txt", want: nil, absent: []string{"x1.txt"}},
		{pattern: "?.txt", want: []string{"c.txt", "x.txt"}, absent: []string{"a.go"}},
		{pattern: "d/**/*.{go,md}", want: []string{"d/e.go", "d/f.md"}, absent: []string{"a.go"}},
		{pattern: "**/{e,f}.go", want: []string{"d/e.go"}, absent: []string{"a.go"}},
	}
	for _, tc := range cases {
		out, e := app.globFiles(context.Background(), GlobInput{Pattern: tc.pattern})
		if e != nil {
			t.Fatalf("%s: %v", tc.pattern, e)
		}
		for _, want := range tc.want {
			if !strings.Contains(out.Output, want) {
				t.Fatalf("pattern %q missed %q: %q", tc.pattern, want, out.Output)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(out.Output, absent) {
				t.Fatalf("pattern %q matched %q: %q", tc.pattern, absent, out.Output)
			}
		}
	}
}

func TestSearchGlobRejectsInvalidPatternAsNoMatch(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "a.go", "x")
	out, e := app.globFiles(context.Background(), GlobInput{Pattern: "src/[unclosed"})
	if e != nil {
		t.Fatalf("an unparseable pattern must not fail the call: %v", e)
	}
	if strings.TrimSpace(out.Output) != "No files found matching pattern." {
		t.Fatalf("invalid pattern must report no match: %q", out.Output)
	}
}

func TestSearchGlobKeepsHiddenFilesOutUnlessNamed(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, ".hidden/secret.txt", "x")
	writeTreeFixture(t, root, "visible.txt", "x")
	out, e := app.globFiles(context.Background(), GlobInput{Pattern: "**/*.txt"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.Output, "visible.txt") || strings.Contains(out.Output, ".hidden") {
		t.Fatalf("hidden paths must stay out of a wildcard glob: %q", out.Output)
	}
	named, e := app.globFiles(context.Background(), GlobInput{Pattern: "**/.hidden/*.txt"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(named.Output, ".hidden/secret.txt") {
		t.Fatalf("a pattern naming the hidden segment must match: %q", named.Output)
	}
}

func TestSearchExplicitIgnoredRootReturnsNoMatches(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "node_modules/pkg/index.js", "x")
	writeTreeFixture(t, root, "keep.js", "x")
	out, e := app.globFiles(context.Background(), GlobInput{Path: "node_modules", Pattern: "**/*.js"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.TrimSpace(out.Output) != "No files found matching pattern." {
		t.Fatalf("an explicitly selected ignored root must not leak files: %q", out.Output)
	}
}

func TestSearchDoesNotFollowSymlinksOutOfTheWorkRoot(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	outside := t.TempDir()
	writeTreeFixture(t, outside, "secret.txt", "needle outside the work root")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	grep, e := app.grepFiles(context.Background(), GrepInput{Pattern: "needle"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(grep.Output, "escape.txt") || strings.Contains(grep.Output, "escape-dir") {
		t.Fatalf("grep followed a symlink out of the work root: %q", grep.Output)
	}
	found, e := app.globFiles(context.Background(), GlobInput{Pattern: "**/*.txt"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(found.Output, "escape-dir/secret.txt") {
		t.Fatalf("glob descended into a symlinked directory: %q", found.Output)
	}
}

func TestGlobUnicodeAndEscapedAlternates(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	for _, name := range []string{"资料/报告中.txt", "资料/报告英.md", "资料/😀.txt", "{a,b}.txt"} {
		writeTreeFixture(t, root, name, "needle")
	}
	for _, tc := range []struct{ pattern, want string }{
		{"资料/报告[中英].{txt,md}", "资料/报告中.txt\n资料/报告英.md\n"},
		{"资料/?.txt", "资料/😀.txt\n"},
		{`\{a,b\}.txt`, "{a,b}.txt\n"},
	} {
		out, err := app.globFiles(context.Background(), GlobInput{Pattern: tc.pattern})
		if err != nil || out.Output != tc.want {
			t.Errorf("glob %q: got %q, %v; want %q", tc.pattern, out.Output, err, tc.want)
		}
	}
	out, err := app.grepFiles(context.Background(), GrepInput{Pattern: "needle", Include: "资料/报告[中英].{txt,md}"})
	if err != nil || !strings.Contains(out.Output, "资料/报告中.txt:1:needle") || !strings.Contains(out.Output, "资料/报告英.md:1:needle") {
		t.Fatalf("Unicode include failed: %q %v", out.Output, err)
	}
}

func TestGlobPatternBudget(t *testing.T) {
	// 512 alternatives is deliberately small enough to safely run against the
	// old eager expander. Never reproduce the 2^25 OOM case in a test.
	for _, pattern := range []string{
		strings.Repeat("{a,b}", 9),
		strings.Repeat("{", 33) + "a,b" + strings.Repeat("}", 33),
		strings.Repeat("a", 4097),
	} {
		if _, err := compileGlobPattern(pattern); err == nil {
			t.Errorf("expected bounded rejection for %d-byte glob", len(pattern))
		}
	}
	for _, pattern := range []string{
		strings.Repeat("{a,b}", 8), // Exactly 256 branches remains accepted.
		strings.Repeat("{", 16) + "a,b" + strings.Repeat("}", 16),
		strings.Repeat("a", 4096),
		`[{}]`, `\{a,b\}`,
	} {
		if _, err := compileGlobPattern(pattern); err != nil {
			t.Errorf("within-budget glob rejected: %v", err)
		}
	}
	app, _, _ := testApp(t)
	out, err := app.grepFiles(context.Background(), GrepInput{Pattern: "needle", Include: strings.Repeat("{a,b}", 9)})
	if err == nil {
		t.Fatalf("over-budget grep include accepted: %q", out.Output)
	}
}

func TestSearchSelectedRootIntermediateSymlinkEscape(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	outside := t.TempDir()
	writeTreeFixture(t, outside, "sub/secret.txt", "OUTSIDE_SENTINEL")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, selected := range []string{"link/sub", filepath.Join(root, "link", "sub")} {
		out, err := app.grepFiles(context.Background(), GrepInput{Path: selected, Pattern: "OUTSIDE_SENTINEL"})
		if err == nil {
			t.Errorf("grep accepted escaped selected root %q: %q", selected, out.Output)
		}
		out, err = app.globFiles(context.Background(), GlobInput{Path: selected, Pattern: "**/*.txt"})
		if err == nil {
			t.Errorf("glob accepted escaped selected root %q: %q", selected, out.Output)
		}
	}
}

func TestSearchRootedOpenRejectsIntermediateReplacement(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	outside := t.TempDir()
	writeTreeFixture(t, root, "safe/sub/data.txt", "inside")
	writeTreeFixture(t, outside, "sub/data.txt", "OUTSIDE_SENTINEL")
	called := false
	err := app.searchWalk(context.Background(), "safe/sub", func(workspace *os.Root, path, rel string, entry os.DirEntry) error {
		if entry.Name() != "data.txt" {
			return nil
		}
		called = true
		// The entry has been discovered; replace its intermediate directory
		// before invoking the same Root.OpenFile operation used by grep.
		from, to := filepath.Join(root, "safe"), filepath.Join(root, "held")
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, from); err != nil {
			t.Fatal(err)
		}
		if file, err := workspace.OpenFile(filepath.FromSlash(rel), os.O_RDONLY, 0); err == nil {
			file.Close()
			t.Fatal("search reopened a replaced entry outside WorkRoot")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("replacement fixture did not exercise search-open: called=%v err=%v", called, err)
	}
}

// TestGrepSingleFilePathMatchesDirectoryMode pins the single-file grep additions:
// an explicit regular file is searched, and its output lines are byte-identical
// to the same file reached through the directory walk.
func TestGrepSingleFilePathMatchesDirectoryMode(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	body := "alpha first\nbeta second\nALPHA third\n"
	writeTreeFixture(t, root, "src/main.go", body)
	for _, tc := range []struct {
		name string
		in   GrepInput
	}{
		{"plain", GrepInput{Path: "src/main.go", Pattern: "ALPHA", IgnoreCase: true}},
		{"context", GrepInput{Path: "src/main.go", Pattern: "beta", Context: 1}},
		{"absolute", GrepInput{Path: filepath.Join(root, "src", "main.go"), Pattern: "beta", Context: 1}},
		{"filesOnly", GrepInput{Path: "src/main.go", Pattern: "alpha", FilesOnly: true}},
		{"maxResults", GrepInput{Path: "src/main.go", Pattern: "alpha", MaxResults: 1}},
		{"include", GrepInput{Path: "src/main.go", Pattern: "alpha", Include: "**/*.go"}},
	} {
		single, e := app.grepFiles(context.Background(), tc.in)
		if e != nil {
			t.Fatalf("%s: single file rejected: %v", tc.name, e)
		}
		dirIn := tc.in
		dirIn.Path = "src"
		walk, e := app.grepFiles(context.Background(), dirIn)
		if e != nil {
			t.Fatalf("%s: directory walk failed: %v", tc.name, e)
		}
		if single.Output != walk.Output {
			t.Fatalf("%s: single-file output %q differs from directory output %q", tc.name, single.Output, walk.Output)
		}
	}
	hit, e := app.grepFiles(context.Background(), GrepInput{Path: "src/main.go", Pattern: "ALPHA", IgnoreCase: true})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(hit.Output, "src/main.go:1:alpha first") || !strings.Contains(hit.Output, "src/main.go:3:ALPHA third") {
		t.Fatalf("single-file grep lost matches: %q", hit.Output)
	}
	ctxOut, e := app.grepFiles(context.Background(), GrepInput{Path: "src/main.go", Pattern: "beta", Context: 1})
	if e != nil {
		t.Fatal(e)
	}
	if want := "src/main.go-1-alpha first\nsrc/main.go:2:beta second\nsrc/main.go-3-ALPHA third\n"; ctxOut.Output != want {
		t.Fatalf("single-file context lines: got %q want %q", ctxOut.Output, want)
	}
}

// TestGrepSingleFilePathNoMatch reports the same empty result as directory mode.
func TestGrepSingleFilePathNoMatch(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "src/main.go", "package p")
	out, e := app.grepFiles(context.Background(), GrepInput{Path: "src/main.go", Pattern: "no-such-token"})
	if e != nil {
		t.Fatalf("single file must not error on no match: %v", e)
	}
	if strings.TrimSpace(out.Output) != "No matches found." {
		t.Fatalf("single-file empty grep: %q", out.Output)
	}
}

// TestGrepSingleFilePathSkipsBinaryExtension keeps the binary-extension rule for
// an explicitly selected file: searched content is never emitted, and the call
// reports no match instead of failing.
func TestGrepSingleFilePathSkipsBinaryExtension(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "assets/logo.png", "needle inside a binary-suffixed file")
	out, e := app.grepFiles(context.Background(), GrepInput{Path: "assets/logo.png", Pattern: "needle"})
	if e != nil {
		t.Fatalf("binary-suffixed single file must not error: %v", e)
	}
	if strings.Contains(out.Output, "needle") || strings.TrimSpace(out.Output) != "No matches found." {
		t.Fatalf("binary-suffixed single file was searched: %q", out.Output)
	}
}

// TestGrepRejectsNonRegularAndEscapingSinglePaths confirms the single-file branch
// did not widen the WorkRoot boundary or accept special files.
func TestGrepRejectsNonRegularAndEscapingSinglePaths(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "src/main.go", "needle")
	outside := t.TempDir()
	writeTreeFixture(t, outside, "secret.txt", "needle")
	fifo := filepath.Join(root, "pipe")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Skipf("FIFOs are unavailable here: %v", e)
	}
	for _, selected := range []string{
		"pipe",
		filepath.Join(root, "pipe"),
		filepath.Join(outside, "secret.txt"),
		"../" + filepath.Base(outside) + "/secret.txt",
	} {
		out, e := app.grepFiles(context.Background(), GrepInput{Path: selected, Pattern: "needle"})
		if e == nil {
			t.Fatalf("grep accepted rejected single path %q: %q", selected, out.Output)
		}
		if strings.Contains(out.Output, "needle") {
			t.Fatalf("rejected path %q leaked content: %q", selected, out.Output)
		}
	}
	for _, link := range []struct{ name, target, selected string }{
		{"escape.txt", filepath.Join(outside, "secret.txt"), "escape.txt"},
		{"linkdir", outside, "linkdir/secret.txt"},
	} {
		if e := os.Symlink(link.target, filepath.Join(root, link.name)); e != nil {
			t.Skipf("cannot create symlink %s: %v", link.name, e)
		}
		out, e := app.grepFiles(context.Background(), GrepInput{Path: link.selected, Pattern: "needle"})
		if e == nil {
			t.Fatalf("grep followed escaping symlink %q: %q", link.selected, out.Output)
		}
		if strings.Contains(out.Output, "needle") {
			t.Fatalf("symlink %q leaked content: %q", link.selected, out.Output)
		}
	}
}

// TestGrepSingleFilePathHonoursSizeBudget proves the single-file branch reuses
// max_search_file_bytes instead of reading an unbounded file.
func TestGrepSingleFilePathHonoursSizeBudget(t *testing.T) {
	root := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, MaxSearchFileBytes: 64, MaxOutputBytes: 128 * 1024}}}
	writeTreeFixture(t, root, "big.txt", strings.Repeat("needle filler line\n", 32))
	writeTreeFixture(t, root, "small.txt", "needle\n")
	out, e := app.grepFiles(context.Background(), GrepInput{Path: "big.txt", Pattern: "needle"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.Output, "needle") {
		t.Fatalf("single-file grep read an oversized file: %q", out.Output)
	}
	if !strings.Contains(out.Output, "skipped 1 oversized files") {
		t.Fatalf("single-file size skip not reported: %q", out.Output)
	}
	if ok, e := app.grepFiles(context.Background(), GrepInput{Path: "small.txt", Pattern: "needle"}); e != nil || !strings.Contains(ok.Output, "small.txt:1:needle") {
		t.Fatalf("within-budget single file lost its hit: %v %q", e, ok.Output)
	}
}

// TestGrepSingleFileIsSearchableInsideIgnoredDirectory pins the deliberate
// asymmetry: ignore rules prune directory walks, but a file the caller names
// explicitly is searched, exactly like the walk's own "selected entry is never
// ignore-filtered" rule. Without this the behaviour would silently drift.
func TestGrepSingleFileIsSearchableInsideIgnoredDirectory(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "node_modules/pkg/index.js", "explicit-needle\n")
	walk, e := app.grepFiles(context.Background(), GrepInput{Path: "node_modules", Pattern: "explicit-needle"})
	if e != nil || !strings.Contains(walk.Output, "No matches found.") {
		t.Fatalf("ignored directory walk changed: %v %q", e, walk.Output)
	}
	named, e := app.grepFiles(context.Background(), GrepInput{Path: "node_modules/pkg/index.js", Pattern: "explicit-needle"})
	if e != nil {
		t.Fatalf("explicitly named ignored file rejected: %v", e)
	}
	if !strings.Contains(named.Output, "node_modules/pkg/index.js:1:explicit-needle") {
		t.Fatalf("explicitly named ignored file not searched: %q", named.Output)
	}
}

// TestGlobKeepsDirectoryOnlySemantics guards the other half of the change: a
// single-file path is still not a glob root.
func TestGlobKeepsDirectoryOnlySemantics(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "src/main.go", "needle")
	out, e := app.globFiles(context.Background(), GlobInput{Path: "src/main.go", Pattern: "**/*.go"})
	if e == nil {
		t.Fatalf("glob accepted a single-file root: %q", out.Output)
	}
	if !strings.Contains(e.Error(), "search path is not a directory") {
		t.Fatalf("glob error changed semantics: %v", e)
	}
}

func TestSearchGrepSkipsBinaryExtensionsAndReportsNoMatch(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	writeTreeFixture(t, root, "notes.txt", "needle here")
	writeTreeFixture(t, root, "logo.png", "needle here")
	out, e := app.grepFiles(context.Background(), GrepInput{Pattern: "needle"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.Output, "notes.txt") {
		t.Fatalf("grep lost a text match: %q", out.Output)
	}
	if strings.Contains(out.Output, "logo.png") {
		t.Fatalf("grep searched a binary extension: %q", out.Output)
	}
	none, e := app.grepFiles(context.Background(), GrepInput{Pattern: "no-such-token-anywhere"})
	if e != nil {
		t.Fatal(e)
	}
	if strings.TrimSpace(none.Output) != "No matches found." {
		t.Fatalf("empty grep must report no matches: %q", none.Output)
	}
}
