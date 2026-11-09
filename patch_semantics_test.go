package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These regressions pin the patch semantics ported from the locked Rust
// upstream (upstream-audit/codex-free-v1.9.1/src/apply_patch.rs): pure
// insertions append at end of file, "*** End of File" anchors a hunk to the
// final position, context matching falls back through an exact ->
// trim_end -> trim -> punctuation-normalised ladder, CRLF endings and the
// final newline survive an update, and an "@@" anchor matches a whole line.

// patchWriteFile writes content into the app work root and returns its path.
func patchWriteFile(t *testing.T, a *App, name, content string) string {
	t.Helper()
	p := filepath.Join(a.config.Get().WorkRoot, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// patchReadFile returns the current contents of a path.
func patchReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// patchRun applies a patch through the registered tool entry point.
func patchRun(t *testing.T, a *App, patch string) error {
	t.Helper()
	_, err := a.applyCodexPatch(context.Background(), patch)
	return err
}

func TestPatchPureInsertAppendsAtEndOfFile(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "notes.txt", "alpha\nbeta\n")
	if err := patchRun(t, a, "*** Begin Patch\n*** Update File: notes.txt\n@@\n+gamma\n*** End Patch"); err != nil {
		t.Fatalf("pure insertion rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "alpha\nbeta\ngamma\n" {
		t.Fatalf("pure insertion did not append at end of file: %q", got)
	}
}

func TestPatchPureInsertAppendsAfterExistingContent(t *testing.T) {
	// A pure insertion is appended to the file even when the hunk carries a
	// context header: upstream resolves the context, then still appends.
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "notes.txt", "alpha\nbeta\n")
	patch := "*** Begin Patch\n*** Update File: notes.txt\n@@ alpha\n+gamma\n*** End Patch"
	if err := patchRun(t, a, patch); err != nil {
		t.Fatalf("pure insertion rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "alpha\nbeta\ngamma\n" {
		t.Fatalf("pure insertion did not append at end of file: %q", got)
	}
}

func TestPatchEndOfFileAnchorSelectsFinalOccurrence(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "dup.txt", "keep\ndup\ndup\n")
	patch := "*** Begin Patch\n*** Update File: dup.txt\n@@\n-dup\n+last\n*** End of File\n*** End Patch"
	if err := patchRun(t, a, patch); err != nil {
		t.Fatalf("end-of-file anchored hunk rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "keep\ndup\nlast\n" {
		t.Fatalf("end-of-file anchor did not select the final occurrence: %q", got)
	}
}

func TestPatchEndOfFileAnchorRejectedWhenHunkIsNotAtEnd(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "head.txt", "head\ntail\n")
	patch := "*** Begin Patch\n*** Update File: head.txt\n@@\n-head\n+HEAD\n*** End of File\n*** End Patch"
	if err := patchRun(t, a, patch); err == nil {
		t.Fatal("end-of-file anchored hunk matched a line that is not at the end of the file")
	}
	if got := patchReadFile(t, p); got != "head\ntail\n" {
		t.Fatalf("rejected patch mutated the file: %q", got)
	}
}

func TestPatchContextMatchingFallsBackThroughFuzzyLadder(t *testing.T) {
	cases := []struct {
		name    string
		content string
		oldLine string
	}{
		{"trailing whitespace", "value = 1;   \n", "value = 1;"},
		{"leading and trailing whitespace", "   value = 1;\t\n", "value = 1;"},
		{"curly double quotes", "let s = \u201Chi\u201D;\n", `let s = "hi";`},
		{"curly single quotes", "let s = \u2018hi\u2019;\n", "let s = 'hi';"},
		{"unicode dash", "a \u2014 b\n", "a - b"},
		{"non breaking space", "a\u00A0b\n", "a b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := testApp(t)
			p := patchWriteFile(t, a, "fuzzy.txt", tc.content)
			patch := "*** Begin Patch\n*** Update File: fuzzy.txt\n@@\n-" + tc.oldLine + "\n+replaced\n*** End Patch"
			if err := patchRun(t, a, patch); err != nil {
				t.Fatalf("fuzzy context rejected: %v", err)
			}
			if got := patchReadFile(t, p); got != "replaced\n" {
				t.Fatalf("unexpected result %q", got)
			}
		})
	}
}

func TestPatchContextMismatchIsRejected(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "strict.txt", "value = 2;\n")
	patch := "*** Begin Patch\n*** Update File: strict.txt\n@@\n-value = 1;\n+replaced\n*** End Patch"
	if err := patchRun(t, a, patch); err == nil {
		t.Fatal("patch applied although the context did not match")
	}
	if got := patchReadFile(t, p); got != "value = 2;\n" {
		t.Fatalf("rejected patch mutated the file: %q", got)
	}
}

func TestPatchPreservesCRLFLineEndings(t *testing.T) {
	a, _, _ := testApp(t)
	crlf := patchWriteFile(t, a, "crlf.txt", "alpha\r\nbeta\r\n")
	if err := patchRun(t, a, "*** Begin Patch\n*** Update File: crlf.txt\n@@\n-beta\n+BETA\n*** End Patch"); err != nil {
		t.Fatalf("CRLF patch rejected: %v", err)
	}
	if got := patchReadFile(t, crlf); got != "alpha\r\nBETA\r\n" {
		t.Fatalf("CRLF endings were not preserved: %q", got)
	}
	lf := patchWriteFile(t, a, "lf.txt", "alpha\nbeta\n")
	if err := patchRun(t, a, "*** Begin Patch\n*** Update File: lf.txt\n@@\n-beta\n+BETA\n*** End Patch"); err != nil {
		t.Fatalf("LF patch rejected: %v", err)
	}
	if got := patchReadFile(t, lf); got != "alpha\nBETA\n" {
		t.Fatalf("LF file gained CRLF endings: %q", got)
	}
}

func TestPatchMixedLineEndingsFollowTheMajority(t *testing.T) {
	// Upstream rewrites the whole file with the dominant ending once the
	// file is predominantly CRLF.
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "mixed.txt", "alpha\r\nbeta\ngamma\r\n")
	if err := patchRun(t, a, "*** Begin Patch\n*** Update File: mixed.txt\n@@\n-beta\n+BETA\n*** End Patch"); err != nil {
		t.Fatalf("mixed ending patch rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "alpha\r\nBETA\r\ngamma\r\n" {
		t.Fatalf("mixed endings not normalised to the dominant ending: %q", got)
	}
}

func TestPatchFinalNewlineHandling(t *testing.T) {
	t.Run("file without trailing newline gains one", func(t *testing.T) {
		a, _, _ := testApp(t)
		p := patchWriteFile(t, a, "nonl.txt", "alpha\nbeta")
		if err := patchRun(t, a, "*** Begin Patch\n*** Update File: nonl.txt\n@@\n-beta\n+BETA\n*** End Patch"); err != nil {
			t.Fatalf("patch rejected: %v", err)
		}
		if got := patchReadFile(t, p); got != "alpha\nBETA\n" {
			t.Fatalf("unexpected final newline handling: %q", got)
		}
	})
	t.Run("removed final newline line still matches", func(t *testing.T) {
		a, _, _ := testApp(t)
		p := patchWriteFile(t, a, "finalnl.txt", "alpha\nbeta\n")
		patch := "*** Begin Patch\n*** Update File: finalnl.txt\n@@\n-beta\n+BETA\n-\n*** End Patch"
		if err := patchRun(t, a, patch); err != nil {
			t.Fatalf("patch rejected: %v", err)
		}
		if got := patchReadFile(t, p); got != "alpha\nBETA\n" {
			t.Fatalf("unexpected final newline handling: %q", got)
		}
	})
}

func TestPatchAnchorRequiresWholeLineMatch(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "anchor.txt", "func handler() {\n    body()\n}\n")
	patch := "*** Begin Patch\n*** Update File: anchor.txt\n@@ handler\n-    body()\n+    changed()\n*** End Patch"
	if err := patchRun(t, a, patch); err == nil {
		t.Fatal("anchor that only matches a substring of a line was accepted")
	}
	if got := patchReadFile(t, p); got != "func handler() {\n    body()\n}\n" {
		t.Fatalf("rejected patch mutated the file: %q", got)
	}
}

func TestPatchAnchorNarrowsTheHunk(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "anchor.txt", "first\nfunc handler() {\n    body()\n}\nlast\n")
	patch := "*** Begin Patch\n*** Update File: anchor.txt\n@@ func handler() {\n-    body()\n+    changed()\n*** End Patch"
	if err := patchRun(t, a, patch); err != nil {
		t.Fatalf("anchor patch rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "first\nfunc handler() {\n    changed()\n}\nlast\n" {
		t.Fatalf("anchor patch produced %q", got)
	}
}

func TestPatchToleratesBlankEnvelopeLines(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "target.txt", "keep\n")
	patch := "\n\n*** Begin Patch\n*** Update File: target.txt\n@@\n-keep\n+changed\n*** End Patch\n\n"
	if err := patchRun(t, a, patch); err != nil {
		t.Fatalf("blank envelope rejected: %v", err)
	}
	if got := patchReadFile(t, p); got != "changed\n" {
		t.Fatalf("unexpected result %q", got)
	}
}

func TestPatchRejectsInvalidGrammar(t *testing.T) {
	a, _, _ := testApp(t)
	p := patchWriteFile(t, a, "target.txt", "keep\n")
	cases := []struct{ name, patch string }{
		{"missing begin marker", "*** Update File: target.txt\n@@\n-keep\n+changed\n*** End Patch"},
		{"missing end marker", "*** Begin Patch\n*** Update File: target.txt\n@@\n-keep\n+changed"},
		{"add hunk line without plus", "*** Begin Patch\n*** Add File: new.txt\nno plus\n*** End Patch"},
		{"update line without prefix", "*** Begin Patch\n*** Update File: target.txt\n@@\nbare\n*** End Patch"},
		{"update without chunks", "*** Begin Patch\n*** Update File: target.txt\n*** End Patch"},
		{"end of file inside add hunk", "*** Begin Patch\n*** Add File: new.txt\n+line\n*** End of File\n*** End Patch"},
		{"move to not directly after update", "*** Begin Patch\n*** Update File: target.txt\n@@\n-keep\n+changed\n*** Move to: other.txt\n*** End Patch"},
		{"empty patch", "*** Begin Patch\n*** End Patch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := patchRun(t, a, tc.patch); err == nil {
				t.Fatal("invalid patch accepted")
			}
		})
	}
	if got := patchReadFile(t, p); got != "keep\n" {
		t.Fatalf("invalid patch mutated the file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(a.config.Get().WorkRoot, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("invalid patch created a file")
	}
}

func TestPatchUpdateRejectsNonUTF8File(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	p := filepath.Join(root, "binary.bin")
	// The file holds a line the patch would match, so the only reason to
	// refuse it is that it is not valid UTF-8 text.
	original := []byte("A\n\xff\xfe")
	if err := os.WriteFile(p, original, 0644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: binary.bin\n@@\n-A\n+B\n*** End Patch"
	if err := patchRun(t, a, patch); err == nil {
		t.Fatal("patch accepted a file that is not valid UTF-8")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(original) {
		t.Fatalf("rejected patch mutated the binary file: %v", b)
	}
}

// --- unit level coverage of the ported upstream primitives -----------------

// TestPatchApplyUpdateMatchesUpstreamFixtures mirrors the Rust unit tests of
// the locked upstream (src/apply_patch.rs) one by one, so the port keeps the
// reference implementation's observable behaviour.
func TestPatchApplyUpdateMatchesUpstreamFixtures(t *testing.T) {
	t.Run("applies update", func(t *testing.T) {
		ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n@@\n one\n-two\n+TWO\n three\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		got, err := applyPatchUpdate("one\ntwo\nthree\n", ops[0].chunks, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got != "one\nTWO\nthree\n" {
			t.Fatalf("unexpected result %q", got)
		}
	})
	t.Run("update hunk without leading context marker", func(t *testing.T) {
		ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n one\n-two\n+TWO\n three\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		if len(ops[0].chunks) != 1 || ops[0].chunks[0].changeContext != "" {
			t.Fatalf("expected one context-less chunk, got %+v", ops[0].chunks)
		}
		got, err := applyPatchUpdate("one\ntwo\nthree\n", ops[0].chunks, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got != "one\nTWO\nthree\n" {
			t.Fatalf("unexpected result %q", got)
		}
	})
	t.Run("fuzzy matches curly quotes", func(t *testing.T) {
		ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n@@\n-let s = \"hi\";\n+let s = \"bye\";\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		got, err := applyPatchUpdate("let s = \u201Chi\u201D;\n", ops[0].chunks, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got != "let s = \"bye\";\n" {
			t.Fatalf("unexpected result %q", got)
		}
	})
	t.Run("preserves crlf", func(t *testing.T) {
		ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n@@\n a\n-b\n+B\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		got, err := applyPatchUpdate("a\r\nb\r\n", ops[0].chunks, "f")
		if err != nil {
			t.Fatal(err)
		}
		if got != "a\r\nB\r\n" {
			t.Fatalf("unexpected result %q", got)
		}
	})
	t.Run("reports the upstream apply errors", func(t *testing.T) {
		ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n@@\n-nope\n+x\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = applyPatchUpdate("one\n", ops[0].chunks, "f"); err == nil {
			t.Fatal("expected an apply error")
		} else if !strings.Contains(err.Error(), "Failed to find expected lines in f") {
			t.Fatalf("unexpected error %v", err)
		}
		ops, err = readCodexPatch("*** Begin Patch\n*** Update File: f\n@@ missing\n-one\n+two\n*** End Patch")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = applyPatchUpdate("one\n", ops[0].chunks, "f"); err == nil {
			t.Fatal("expected a context error")
		} else if !strings.Contains(err.Error(), "Failed to find context 'missing' in f") {
			t.Fatalf("unexpected error %v", err)
		}
	})
}

func TestPatchRejectsOverlappingEOFChunksWithoutMutation(t *testing.T) {
	a, _, _ := testApp(t)
	path := filepath.Join(a.config.Get().WorkRoot, "overlap.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if value := recover(); value != nil {
			t.Fatalf("overlapping EOF chunks panicked: %v", value)
		}
	}()
	patch := "*** Begin Patch\n*** Update File: overlap.txt\n@@\n-a\n-b\n-c\n@@\n-a\n-b\n-c\n*** End of File\n*** End Patch"
	if _, err := a.applyCodexPatch(t.Context(), patch); err == nil {
		t.Fatal("overlapping chunks were accepted")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "a\nb\nc\n" {
		t.Fatalf("rejected patch mutated original: %q, %v", got, err)
	}
}

func TestPatchParseUpstreamGrammar(t *testing.T) {
	ops, err := readCodexPatch("*** Begin Patch\n*** Add File: a.txt\n+hello\n+world\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].kind != "add" || ops[0].path != "a.txt" {
		t.Fatalf("unexpected ops %+v", ops)
	}
	if strings.Join(ops[0].lines, "|") != "hello|world" {
		t.Fatalf("unexpected add body %v", ops[0].lines)
	}
}

func TestPatchParseChunkStructure(t *testing.T) {
	ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n@@ func main() {\n ctx\n-old\n+new\n*** End of File\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].kind != "update" || len(ops[0].chunks) != 1 {
		t.Fatalf("unexpected ops %+v", ops)
	}
	c := ops[0].chunks[0]
	if c.changeContext != "func main() {" {
		t.Fatalf("unexpected change context %q", c.changeContext)
	}
	if strings.Join(c.oldLines, "|") != "ctx|old" || strings.Join(c.newLines, "|") != "ctx|new" {
		t.Fatalf("unexpected chunk lines %+v", c)
	}
	if !c.isEndOfFile {
		t.Fatal("end of file flag not parsed")
	}
}

func TestPatchParseSynthesisesContextlessChunk(t *testing.T) {
	ops, err := readCodexPatch("*** Begin Patch\n*** Update File: f\n ctx\n-old\n+new\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops[0].chunks) != 1 || ops[0].chunks[0].changeContext != "" {
		t.Fatalf("expected one contextless chunk, got %+v", ops[0].chunks)
	}
}

func TestPatchSeekSequenceLadder(t *testing.T) {
	lines := []string{"first", "  spaced  ", "third"}
	if got := seekSequence(lines, []string{"first"}, 0, false); got != 0 {
		t.Fatalf("exact match not found at 0: %d", got)
	}
	if got := seekSequence(lines, []string{"spaced"}, 0, false); got != 1 {
		t.Fatalf("trim match not found at 1: %d", got)
	}
	if got := seekSequence(lines, []string{"missing"}, 0, false); got != -1 {
		t.Fatalf("unexpected match for missing line: %d", got)
	}
	if got := seekSequence(lines, []string{"third"}, 0, true); got != 2 {
		t.Fatalf("end of file anchor not honoured: %d", got)
	}
	if got := seekSequence(lines, []string{"first"}, 0, true); got != -1 {
		t.Fatalf("end of file anchor accepted a non final position: %d", got)
	}
}

func TestPatchRenderAddedFile(t *testing.T) {
	if got := renderAddedFile([]string{"x"}); got != "x\n" {
		t.Fatalf("unexpected rendered file %q", got)
	}
	if got := renderAddedFile(nil); got != "" {
		t.Fatalf("expected empty file, got %q", got)
	}
}

func TestPatchUsesCRLFDetection(t *testing.T) {
	if !usesCRLF("a\r\nb\r\n") {
		t.Fatal("CRLF file not detected")
	}
	if usesCRLF("a\nb\n") {
		t.Fatal("LF file reported as CRLF")
	}
	if !usesCRLF("a\r\nb\nc\r\n") {
		t.Fatal("CRLF majority not detected")
	}
}
