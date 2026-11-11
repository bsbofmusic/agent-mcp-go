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
