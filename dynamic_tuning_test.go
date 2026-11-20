package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testPointer[T any](value T) *T { return &value }

// Yield bounds, annotations_mode and bootstrap_files are hot settings that share
// one validation path with config loading: a valid update must be visible in
// tuningText immediately, and every rejected update must leave the live config
// untouched.
func TestDynamicYieldAnnotationsAndBootstrapValidation(t *testing.T) {
	a, srv, token := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, a, srv, token, "/mcp")
	callSettings := func(t *testing.T, name string, args map[string]any) string {
		t.Helper()
		text := handoverOK(t, ctx, client, name, args, "")
		var out TextOut
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode %s response: %v", name, err)
		}
		return out.Output
	}
	root := a.config.Get().WorkRoot
	bootstrap := filepath.Join(root, "bootstrap.md")
	defaults := callSettings(t, "settings_get", map[string]any{})
	if !containsAll(defaults, "annotations_mode=default (default=default, allowed=default|trusted)", "bootstrap_files=0 paths=[] (default=[], work_root-contained absolute paths)") {
		t.Errorf("settings_get did not expose defaults:\n%s", defaults)
	}
	updated := callSettings(t, "settings_update", map[string]any{
		"default_yield_ms": 25000, "max_yield_ms": 45000, "annotations_mode": "  TRUSTED  ", "bootstrap_files": []string{" " + bootstrap + " "},
	})
	if !containsAll(updated,
		"default_yield_ms=25000 (default=10000, range=250..60000, <=max_yield_ms)",
		"max_yield_ms=45000 (default=30000, range=1000..120000, >=default_yield_ms)",
		"annotations_mode=trusted (default=default, allowed=default|trusted)",
		"bootstrap_files=1 paths=["+bootstrap+"] (default=[], work_root-contained absolute paths)",
	) {
		t.Errorf("settings_update did not report the active configuration:\n%s", updated)
	}

	valid := callSettings(t, "settings_get", map[string]any{})
	validConfig, err := os.ReadFile(a.config.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"default under minimum", map[string]any{"default_yield_ms": 249}, "default_yield_ms"},
		{"max under minimum", map[string]any{"default_yield_ms": 250, "max_yield_ms": 600}, "max_yield_ms"},
		{"default over max", map[string]any{"default_yield_ms": 50000, "max_yield_ms": 30000}, "must not exceed"},
		{"unknown annotations", map[string]any{"annotations_mode": "invalid"}, "annotations_mode"},
		{"relative bootstrap", map[string]any{"bootstrap_files": []string{"bootstrap.md"}}, "absolute paths"},
		{"escaping bootstrap", map[string]any{"bootstrap_files": []string{filepath.Join(root, "..", "outside-bootstrap.md")}}, "stay inside work_root"},
		{"empty bootstrap path", map[string]any{"bootstrap_files": []string{""}}, "absolute paths"},
		{"NUL in bootstrap path", map[string]any{"bootstrap_files": []string{filepath.Join(root, "bad\x00.md")}}, "must not contain NUL"},
		{"bootstrap path cleaning and trimming expose parent", map[string]any{"bootstrap_files": []string{filepath.Join(root, ".. /.")}}, "stay inside work_root"},
		{"bootstrap path cleaning exposes filename whitespace", map[string]any{"bootstrap_files": []string{root + "/notes.md /."}}, "must not resolve to leading or trailing whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := handoverCall(t, ctx, client, "settings_update", tc.args, "")
			if !res.IsError || !strings.Contains(toolText(res), tc.want) {
				t.Errorf("settings_update error = %v, %q, want %q", res.IsError, toolText(res), tc.want)
			}
			if current := callSettings(t, "settings_get", map[string]any{}); current != valid {
				t.Errorf("rejected update changed the active settings")
			}
			if saved, err := os.ReadFile(a.config.path); err != nil || string(saved) != string(validConfig) {
				t.Errorf("rejected update did not preserve the persisted config (read error: %v)", err)
			}
		})
	}

	boot := filepath.Join(root, "boot.md")
	wantBootstrap := "bootstrap_files=1 paths=[" + boot + "] (default=[], work_root-contained absolute paths)"
	if updated := callSettings(t, "settings_update", map[string]any{"bootstrap_files": []string{root + "/sub/../boot.md"}}); !strings.Contains(updated, wantBootstrap) {
		t.Fatalf("settings_update did not normalize bootstrap path:\n%s", updated)
	}
	if updated := callSettings(t, "settings_update", map[string]any{"max_output_bytes": 4096}); !strings.Contains(updated, wantBootstrap) {
		t.Fatalf("unrelated settings_update changed bootstrap path:\n%s", updated)
	}
	reloaded, err := readConfig(a.config.path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.BootstrapFiles) != 1 || reloaded.BootstrapFiles[0] != boot {
		t.Errorf("reloaded bootstrap_files = %q, want [%q]", reloaded.BootstrapFiles, boot)
	}
}

func TestDynamicConfigSliceIsolation(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	store := &ConfigStore{
		path: filepath.Join(root, "config.json"),
		cfg:  Config{WorkRoot: root, AllowedOrigins: []string{"https://example.org"}, ExecutableDirs: []string{bin}},
	}
	path := filepath.Join(root, "bootstrap.md")
	files := []string{path}
	updated, err := store.Update(SettingsInput{BootstrapFiles: &files})
	if err != nil {
		t.Fatal(err)
	}
	files[0] = "/outside"
	updated.BootstrapFiles[0] = "/outside"
	updated.AllowedOrigins[0] = "*"
	updated.ExecutableDirs[0] = "/changed-update"
	first := store.Get()
	if first.BootstrapFiles[0] != path || first.AllowedOrigins[0] != "https://example.org" || first.ExecutableDirs[0] != bin {
		t.Error("settings update exposes the store's slice backing arrays")
	}
	first.BootstrapFiles[0] = "/outside"
	first.AllowedOrigins[0] = "*"
	first.ExecutableDirs[0] = "/changed-get"
	second := store.Get()
	if second.BootstrapFiles[0] != path || second.AllowedOrigins[0] != "https://example.org" || second.ExecutableDirs[0] != bin {
		t.Error("settings read exposes the store's slice backing arrays")
	}

	original := []string{" " + path + " "}
	snapshot := Config{WorkRoot: second.WorkRoot, BootstrapFiles: original}
	if err := snapshot.normalizeAndValidateDynamicFields(); err != nil {
		t.Fatal(err)
	}
	if original[0] != " "+path+" " || snapshot.BootstrapFiles[0] != path || snapshot.AnnotationsMode != "default" {
		t.Error("normalization mutated its input slice or did not apply defaults")
	}
}

func TestDynamicSessionYieldBoundsApplyToRunningSession(t *testing.T) {
	a, srv, token := testApp(t)
	t.Cleanup(func() {
		if err := a.sessions.shutdown(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, a, srv, token, "/mcp")
	call := func(t *testing.T, name string, args map[string]any) CommandOutput {
		t.Helper()
		res := handoverCall(t, ctx, client, name, args, "")
		if res.IsError {
			t.Fatalf("%s failed: %s", name, toolText(res))
		}
		data, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var out CommandOutput
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	session := call(t, "exec_command", map[string]any{"cmd": "read value", "yield_time_ms": 250})
	if session.Finished || session.SessionID == 0 {
		t.Fatalf("expected running shell session, got %+v", session)
	}
	// max_yield_ms has a 1000ms settings floor. Use the smallest legal bound.
	handoverOK(t, ctx, client, "settings_update", map[string]any{"default_yield_ms": 450, "max_yield_ms": 1000}, "")
	for _, tc := range []struct {
		name     string
		yield    int
		min, max time.Duration
	}{
		{"below minimum", 1, 250 * time.Millisecond, 420 * time.Millisecond},
		{"omitted uses configured default", 0, 450 * time.Millisecond, 850 * time.Millisecond},
		{"clamped to maximum", 5000, time.Second, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"session_id": session.SessionID}
			if tc.yield > 0 {
				args["yield_time_ms"] = tc.yield
			}
			started := time.Now()
			if out := call(t, "write_stdin", args); out.Finished || out.SessionID != session.SessionID {
				t.Errorf("poll did not leave the same session running: %+v", out)
			}
			if got := time.Since(started); got < tc.min || got > tc.max {
				t.Errorf("poll took %v, want between %v and %v", got, tc.min, tc.max)
			}
		})
	}
}

func TestDynamicSessionYieldCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defaultMS, maxMS := 10000, 30000
		manager := NewSessionManager()
		manager.yieldBounds = func() (int, int) { return defaultMS, maxMS }
		session := &Session{
			manager: manager,
			started: time.Now(),
			output:  &boundedBuffer{},
			done:    make(chan struct{}),
		}
		// Exercise a callback bound below the public settings range.
		defaultMS, maxMS = 450, 600
		for _, tc := range []struct {
			requested int
			want      time.Duration
		}{
			{1, 250 * time.Millisecond},
			{0, 450 * time.Millisecond},
			{5000, 600 * time.Millisecond},
		} {
			start := time.Now()
			out, err := session.waitContext(t.Context(), tc.requested)
			if err != nil || out.Finished {
				t.Fatalf("expected running session, got %+v, %v", out, err)
			}
			if got := time.Since(start); got != tc.want {
				t.Errorf("yield_time_ms=%d waited %v, want %v", tc.requested, got, tc.want)
			}
		}
	})
}

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
