package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCanonicalSkillsStructuredAndPaged(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().SkillsRoot
	dir := filepath.Join(root, "demo")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	body := "---\nname: demo\ndescription: Understand a test skill\n---\n# Header\nLine A\nLine B\n"
	if e := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	list, e := app.listCanonicalSkills("demo")
	if e != nil {
		t.Fatal(e)
	}
	if len(list.Skills) != 1 || list.Skills[0].Name != "demo" || list.Skills[0].Description != "Understand a test skill" {
		t.Fatalf("unexpected structured skill summary %#v", list.Skills)
	}
	page, e := app.readCanonicalSkill(SkillReadInput{Name: "demo", Offset: 3, Limit: 2})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(page.Content, "---\n# Header\n") || strings.Contains(page.Content, "4\t---") {
		t.Fatalf("skills paging failed %q", page.Content)
	}
	if !strings.HasPrefix(page.Content, "demo — "+filepath.Join(dir, "SKILL.md")+"\n\n") {
		t.Fatalf("missing CodexFree skill header %q", page.Content)
	}
	full, e := app.readCanonicalSkill(SkillReadInput{Name: "demo"})
	if e != nil || !strings.Contains(full.Content, "---\nname: demo\n") {
		t.Fatalf("skill Markdown lost its original YAML frontmatter: %v, %q", e, full.Content)
	}
	external := filepath.Join(app.config.Get().WorkRoot, "external.txt")
	if e := os.WriteFile(external, []byte("private"), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "outside")
	if e := os.Symlink(external, link); e != nil {
		t.Fatal(e)
	}
	if _, e := app.readCanonicalSkill(SkillReadInput{Name: "demo", Resource: "outside"}); e == nil {
		t.Fatal("external symlink escape should be rejected")
	}
	// Relative internal symlinks inside the skill package remain readable under os.Root.
	refDir := filepath.Join(dir, "references")
	if e := os.MkdirAll(refDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(refDir, "guide.md"), []byte("internal guide\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join("references", "guide.md"), filepath.Join(dir, "alias.md")); e != nil {
		t.Fatal(e)
	}
	aliasOut, e := app.readCanonicalSkill(SkillReadInput{Name: "demo", Resource: "alias.md"})
	if e != nil || !strings.Contains(aliasOut.Content, "internal guide") {
		t.Fatalf("relative internal symlink should work under os.Root: %v %q", e, aliasOut.Content)
	}
	// Named pipes (FIFOs) must fail immediately via O_NONBLOCK + IsRegular without hanging.
	fifoPath := filepath.Join(dir, "pipe.md")
	if e := syscall.Mkfifo(fifoPath, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := app.readCanonicalSkill(SkillReadInput{Name: "demo", Resource: "pipe.md"}); e == nil {
		t.Fatal("FIFO resource must be rejected as non-regular")
	}
	fifoSkillDir := filepath.Join(root, "fifo-skill")
	if e := os.MkdirAll(fifoSkillDir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := syscall.Mkfifo(filepath.Join(fifoSkillDir, "SKILL.md"), 0600); e != nil {
		t.Fatal(e)
	}
	if listAfterFifo, e := app.listCanonicalSkills(""); e != nil {
		t.Fatal(e)
	} else {
		for _, sk := range listAfterFifo.Skills {
			if sk.Name == "fifo-skill" {
				t.Fatal("FIFO SKILL.md should be skipped by listCanonicalSkills")
			}
		}
	}
}

func TestCanonicalSkillsSearchMatchesDescription(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "document-tool")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	write := func(desc string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("---\nname: document-tool\ndescription: "+desc+"\n---\nbody-only-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Create Excel .xlsx files and 中文表格")
	app := &App{config: &ConfigStore{cfg: Config{SkillsRoot: root, MaxOutputBytes: 4096}}}
	check := func(query string, want int) {
		t.Helper()
		out, err := app.listCanonicalSkills(query)
		if err != nil || len(out.Skills) != want {
			t.Errorf("query %q: got %d matches, want %d (err=%v)", query, len(out.Skills), want, err)
		}
	}
	for _, q := range []string{"", "document-tool", "DOCUMENT", "xlsx", "EXCEL", "中文表格"} {
		check(q, 1)
	}
	for _, q := range []string{"unknown", "body-only-token"} {
		check(q, 0)
	}
	write("Create presentation .pptx files")
	check("xlsx", 0)
	check("pptx", 1)
}

func TestSkillsCatalogCacheHitEmptyAndSameSizeEditInvalidation(t *testing.T) {
	root := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{SkillsRoot: root}}}
	list := func() []SkillInfo {
		t.Helper()
		out, err := app.listCanonicalSkills("")
		if err != nil {
			t.Fatal(err)
		}
		return out.Skills
	}
	checkCounts := func(parses, hits uint64) {
		t.Helper()
		gotHits, gotParses, _ := app.skillsCacheStats()
		if gotParses != parses || gotHits != hits {
			t.Errorf("cache counts = (%d parses, %d hits), want (%d parses, %d hits)", gotParses, gotHits, parses, hits)
		}
	}
	for i := 0; i < 2; i++ {
		if got := list(); len(got) != 0 {
			t.Fatalf("expected empty skill catalog, got %v", got)
		}
		checkCounts(1, uint64(i))
	}

	dir := filepath.Join(root, "demo")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	write := func(desc string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("---\nname: demo\ndescription: "+desc+"\n---\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	checkDescription := func(got []SkillInfo, want string) {
		t.Helper()
		if len(got) != 1 || got[0].Description != want {
			t.Fatalf("catalog = %+v, want one skill with description %q", got, want)
		}
	}
	write("alpha")
	_, _, opensBefore := app.skillsCacheStats()
	first := list()
	checkDescription(first, "alpha")
	checkCounts(2, 1)
	_, _, opensAfter := app.skillsCacheStats()
	if opensAfter != opensBefore+1 {
		t.Errorf("first populated cache miss opened SKILL.md %d times, want 1", opensAfter-opensBefore)
	}
	first[0].Description = "mutated by caller on miss"
	cached := list()
	checkDescription(cached, "alpha")
	checkCounts(2, 2)
	// A cache hit compares only inode/size/mtime/ctime, so it opens no file.
	// This assertion used to require one bounded frontmatter read per hit; the
	// stat fast path replaced that read without weakening invalidation (the
	// same-size, mtime-restored edits below still reparse).
	_, _, opensOnHit := app.skillsCacheStats()
	if opensOnHit != opensAfter {
		t.Errorf("populated cache hit opened SKILL.md %d times, want 0", opensOnHit-opensAfter)
	}
	cached[0].Description = "mutated by caller on hit"
	checkDescription(list(), "alpha")
	checkCounts(2, 3)

	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write("bravo")
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	edited, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Size() != original.Size() || !edited.ModTime().Equal(original.ModTime()) {
		t.Fatal("fixture failed to preserve file size and mtime")
	}
	checkDescription(list(), "bravo")
	checkCounts(3, 3)

	const boundary = 64 * 1024
	const header = "---\ndescription: boundary\n# padding "
	padding := strings.Repeat("a", boundary-3-len(header)-1)
	writeBoundary := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The three hyphens fit in the budget, but the x on the same line does not.
	writeBoundary(header + padding + "\n---x\nbody\n")
	checkDescription(list(), "")
	checkCounts(4, 3)
	original, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Move the fence one byte earlier so its newline is inside the budget.
	writeBoundary(header + padding[:len(padding)-1] + "\n---\n\n\nbody\n")
	if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	edited, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Size() != original.Size() || !edited.ModTime().Equal(original.ModTime()) {
		t.Fatal("boundary fixture failed to preserve file size and mtime")
	}
	checkDescription(list(), "boundary")
	checkCounts(5, 3)
}

func TestListDirectoryIsolatesAbsoluteAndDanglingSymlinks(t *testing.T) {
	app, srv, token := testApp(t)
	root := app.config.Get().WorkRoot
	ctx := context.Background()
	session := handoverHTTPClient(t, ctx, app, srv, token, "/mcp")
	if err := os.WriteFile(filepath.Join(root, "normal.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	// Absolute symlink (mirrors NixOS ~/.nix-profile -> /nix/store/... or /home/agent/...).
	if err := os.Symlink(filepath.Join(root, "normal.txt"), filepath.Join(root, ".nix-profile")); err != nil {
		t.Fatal(err)
	}
	// Dangling relative symlink.
	if err := os.Symlink("missing-target", filepath.Join(root, "dangling-link")); err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_directory",
		Arguments: map[string]any{"path": ""},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_directory failed on directory with absolute/dangling symlinks: err=%v res=%#v", err, res)
	}
	txt := toolText(res)
	if !strings.Contains(txt, "file\t5B\tnormal.txt") {
		t.Fatalf("missing normal.txt in list_directory output: %q", txt)
	}
	if !strings.Contains(txt, "symlink\t-\t.nix-profile") {
		t.Fatalf("expected absolute symlink .nix-profile to degrade cleanly: %q", txt)
	}
	if !strings.Contains(txt, "symlink\t-\tdangling-link") {
		t.Fatalf("expected dangling symlink to degrade cleanly: %q", txt)
	}
}

func TestInitialInstructionsReferenceAMGConfigTruth(t *testing.T) {
	app, _, _ := testApp(t)
	brief := app.buildAgentInstructions()
	if !strings.Contains(brief, "agent-mcp-go-operations") || !strings.Contains(brief, "docs/configuration.md") {
		t.Fatalf("expected AMG config truth pointer in initial instructions, got: %q", brief)
	}
}
