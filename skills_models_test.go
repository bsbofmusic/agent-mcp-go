package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}
