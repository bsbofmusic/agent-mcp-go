package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestSkill(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// Initialization must ship a bounded Skills index (per-entry truncation plus a
// section budget) while get_agent_brief keeps the complete descriptions.
func TestInitializeInstructionsTrimCatalogButBriefKeepsFullDescriptions(t *testing.T) {
	app, _, _ := testApp(t)
	cfg := app.config.Get()
	if err := os.MkdirAll(cfg.SkillsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	longDescription := strings.Repeat("长", 300)
	names := []string{"owner-00", "owner-01", "owner-02", "owner-03", "owner-04", "owner-05", "owner-06", "owner-07"}
	for _, name := range names {
		writeTestSkill(t, cfg.SkillsRoot, name, longDescription)
	}
	entryBytes, maxBytes := 80, 1024
	if _, err := app.config.Update(SettingsInput{SkillIndexEntryBytes: &entryBytes, SkillIndexMaxBytes: &maxBytes}); err != nil {
		t.Fatal(err)
	}

	compact := app.buildInitializeInstructions()
	full := app.buildAgentInstructions()

	if !strings.Contains(compact, "skills_list") {
		t.Fatal("initialization index does not point at skills_list for full descriptions")
	}
	if !strings.Contains(compact, "…") {
		t.Fatal("initialization index did not truncate long descriptions with an ellipsis")
	}
	if strings.Contains(compact, longDescription) {
		t.Fatal("initialization index kept a full long description")
	}
	// The compact section is capped, so only the leading entries survive there;
	// the full brief must still carry every name and every full description.
	if !strings.Contains(compact, "- "+names[0]+" (user)") {
		t.Fatalf("initialization index lost its first skill %s", names[0])
	}
	for _, name := range names {
		if !strings.Contains(full, "- "+name+" (user) — "+longDescription+"\n") {
			t.Fatalf("get_agent_brief context lost the full description of %s", name)
		}
	}
	if !strings.Contains(compact, "[Skills index preview:") {
		t.Fatal("initialization index did not apply skill_index_max_bytes")
	}
	if len(compact) >= len(full) {
		t.Fatalf("initialization instructions (%d bytes) are not smaller than the full brief (%d bytes)", len(compact), len(full))
	}
	indexStart := strings.Index(compact, "\n## Available Skills\n\n")
	indexEnd := strings.Index(compact, "\n## Project instructions")
	if indexStart < 0 {
		t.Fatal("initialization index section is missing")
	}
	if indexEnd < 0 {
		indexEnd = len(compact)
	}
	// The note is fixed overhead; the entries themselves stop at the budget.
	if section := compact[indexStart:indexEnd]; len(section) > maxBytes+1024 {
		t.Fatalf("initialization Skills section = %d bytes, budget %d plus the fixed note", len(section), maxBytes)
	}
}
