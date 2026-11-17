package main

import (
	"context"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPointer[T any](value T) *T { return &value }

func TestDynamicTuningPartialUpdate(t *testing.T) {
	a, _, _ := testApp(t)
	before := a.config.Get()
	next, err := a.config.Update(SettingsInput{MaxSearchEntries: testPointer(12345), MaxShellSessions: testPointer(3)})
	if err != nil {
		t.Fatal(err)
	}
	if next.MaxSearchEntries != 12345 || next.MaxShellSessions != 3 || next.MaxOutputBytes != before.MaxOutputBytes || next.CommandTimeoutSec != before.CommandTimeoutSec {
		t.Fatal("partial update lost settings")
	}
	saved, err := readConfig(a.config.path)
	if err != nil || saved.MaxSearchEntries != 12345 {
		t.Fatalf("not persisted: %v", err)
	}
	for _, in := range []SettingsInput{{MaxShellSessions: testPointer(0)}, {MaxSearchDepth: testPointer(257)}, {MaxOutputBytes: testPointer(0)}, {MaxSearchFileBytes: testPointer(1)}, {MaxFileBytes: testPointer(int64(-1))}} {
		if _, err := a.config.Update(in); err == nil {
			t.Fatal("invalid setting accepted")
		}
	}
	if a.config.Get().MaxSearchEntries != 12345 || a.config.Get().MaxShellSessions != 3 {
		t.Fatal("invalid update changed live config")
	}
}
func TestDynamicSkillBodyAndSupportFiles(t *testing.T) {
	a, _, _ := testApp(t)
	dir := filepath.Join(a.config.Get().SkillsRoot, "large-guide")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: large-guide\ndescription: " + strings.Repeat("中文", 1000) + "\n---\n" + strings.Repeat("line of office instructions\n", 1300) + "END_MARKER\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("support-%02d.md", i)), []byte("support"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := a.readCanonicalSkill(SkillReadInput{Name: "large-guide"})
	if err != nil || !containsAll(out.Content, "END_MARKER", "support-39.md") {
		t.Fatalf("full read failed: %v", err)
	}
	out, err = a.readCanonicalSkill(SkillReadInput{Name: "large-guide", Limit: 1200})
	if err != nil || !strings.Contains(out.Content, "offset=1200") || strings.Contains(out.Content, "END_MARKER") {
		t.Fatal("explicit pagination failed")
	}
	if _, err := a.config.Update(SettingsInput{MaxOutputBytes: testPointer(1024)}); err != nil {
		t.Fatal(err)
	}
	// A single over-budget line must fail rather than return a non-advancing cursor.
	if _, err := a.readCanonicalSkill(SkillReadInput{Name: "large-guide", Offset: 2}); err == nil {
		t.Fatal("oversized line accepted")
	}
}
func TestDynamicSearchBeyondLegacyLimits(t *testing.T) {
	root := t.TempDir()
	a := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, MaxOutputBytes: 512 * 1024}}}
	for i := 0; i < 2100; i++ {
		name := filepath.Join(root, fmt.Sprintf("file-%04d.txt", i))
		if err := os.WriteFile(name, []byte("search-token\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	g, err := a.globFiles(context.Background(), GlobInput{Pattern: "*.txt"})
	if err != nil || !strings.Contains(g.Output, "file-2099.txt") {
		t.Fatalf("glob truncated: %v", err)
	}
	q, err := a.grepFiles(context.Background(), GrepInput{Pattern: "search-token", MaxResults: 0})
	if err != nil || !strings.Contains(q.Output, "file-2099.txt") {
		t.Fatalf("grep truncated: %v", err)
	}
}

func TestDynamicMCPModuleFullInventory(t *testing.T) {
	const url = "http://fixture.invalid"
	tools := make([]map[string]any, 75)
	for i := range tools {
		tools[i] = map[string]any{"name": fmt.Sprintf("tool_%03d", i), "description": "source capability"}
	}
	f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "fixture", "path": "/mcp/fixture", "url": url, "description": "unique module capability"}}, Tools: map[string][]map[string]any{url: tools}})
	for _, limit := range []int{0, 60, 100} {
		got, err := f.app.searchRouteTools(context.Background(), SearchMCPInput{Source: "fixture", Limit: limit})
		want := 75
		if limit > 0 && limit < want {
			want = limit
		}
		if err != nil || len(got.Matches) != want || got.Total != 75 {
			t.Fatalf("limit %d returned %d of %d: %v", limit, len(got.Matches), got.Total, err)
		}
	}
}
func TestDynamicReviewBeyondLegacyCount(t *testing.T) {
	var stats strings.Builder
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&stats, "1\t2\tf-%03d.txt\x00", i)
	}
	out, err := parseReviewStats(stats.String())
	if err != nil || len(out.Files) != 250 || out.FilesOmitted != 0 {
		t.Fatalf("review truncated: %v", err)
	}
}

func TestDynamicArgvCommandAndSearchBudget(t *testing.T) {
	a, _, _ := testApp(t)
	out, failed, err := a.parityRunCommand(context.Background(), RunCommandInput{Command: "printf", Args: []string{"%s", "DYNAMIC_ARGV"}, Timeout: 1000})
	if err != nil || failed || !strings.Contains(out.Output, "DYNAMIC_ARGV") {
		t.Fatalf("dynamic PATH command failed: %v %s", err, out.Output)
	}
	for _, name := range []string{"../printf", ".", "..", "/bin/sh"} {
		if _, _, err := a.parityRunCommand(context.Background(), RunCommandInput{Command: name}); err == nil {
			t.Fatalf("escaped executable accepted: %q", name)
		}
	}
	root := a.config.Get().WorkRoot
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("budget-%d.txt", i)), []byte("budget"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.config.Update(SettingsInput{MaxSearchEntries: testPointer(1)}); err != nil {
		t.Fatal(err)
	}
	g, err := a.globFiles(context.Background(), GlobInput{Pattern: "budget-*.txt"})
	if err != nil || !strings.Contains(g.Output, "[search limit reached]") {
		t.Fatalf("incomplete search not marked: %v %q", err, g.Output)
	}
	if _, err := a.config.Update(SettingsInput{MCPDiscoveryTimeoutSeconds: testPointer(121)}); err == nil {
		t.Fatal("helper-incompatible deadline accepted")
	}
}

func TestDynamicSettingsMCPPartialAndParallelPlan(t *testing.T) {
	a, _, _ := testApp(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "dynamic-tuning", Version: "1"}, nil)
	a.registerCore(server)
	a.registerParity(server)
	client := contractClient(t, server)
	before := a.config.Get()
	got := coreBoundaryCall(t, client, "settings_update", map[string]any{"max_image_bytes": int(8 << 20)})
	if got.IsError || a.config.Get().MaxImageBytes != 8<<20 || a.config.Get().MaxOutputBytes != before.MaxOutputBytes {
		t.Fatalf("partial update failed: %s", toolText(got))
	}
	got = coreBoundaryCall(t, client, "settings_update", map[string]any{"mcp_scan_workers": 0, "max_image_bytes": 1 << 20})
	if !got.IsError || a.config.Get().MaxImageBytes != 8<<20 {
		t.Fatal("invalid patch applied partially")
	}
	got = coreBoundaryCall(t, client, "settings_get", map[string]any{})
	if got.IsError || !strings.Contains(toolText(got), "max_image_bytes=8388608") {
		t.Fatalf("settings not exposed: %s", toolText(got))
	}
	steps := make([]map[string]any, 100)
	for i := range steps {
		steps[i] = map[string]any{"step": fmt.Sprintf("parallel-%d", i), "status": "in_progress"}
	}
	got = coreBoundaryCall(t, client, "update_plan", map[string]any{"scope": "dynamic-tuning-test", "plan": steps})
	if got.IsError || !strings.Contains(toolText(got), "parallel-99") {
		t.Fatalf("parallel plan failed: %s", toolText(got))
	}
}
