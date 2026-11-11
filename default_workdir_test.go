package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeTestConfig persists a Config as an on-disk configuration file.
func writeTestConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// baseTestConfig returns a valid configuration rooted at workRoot. The
// temporary home keeps the fixture hermetic: no live AGENTS.md, token or
// state file of the host is read or written.
func baseTestConfig(t *testing.T, workRoot, defaultWorkdir string) Config {
	t.Helper()
	home := t.TempDir()
	tokenFile := filepath.Join(home, "token")
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("w", 48)), 0600); err != nil {
		t.Fatal(err)
	}
	return Config{
		Listen:            "127.0.0.1:3100",
		WorkRoot:          workRoot,
		DefaultWorkdir:    defaultWorkdir,
		SkillsRoot:        filepath.Join(home, "skills"),
		StateDir:          filepath.Join(home, "state"),
		TokenFile:         tokenFile,
		MaxFileBytes:      300 * 1024 * 1024,
		MaxOutputBytes:    128 * 1024,
		CommandTimeoutSec: 60,
	}
}

// newTestAppWithConfig starts an App from an in-memory configuration file.
func newTestAppWithConfig(t *testing.T, cfg Config) *App {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, cfg)
	app, err := newApp(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// TestDefaultWorkdirConfigValidation covers the out-of-the-box default
// working directory contract at configuration load time.
func TestDefaultWorkdirConfigValidation(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	regularFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	base := baseTestConfig(t, root, project)

	writeTestConfig(t, cfgPath, base)
	got, err := readConfig(cfgPath)
	if err != nil {
		t.Fatalf("valid default_workdir rejected: %v", err)
	}
	if got.DefaultWorkdir != project {
		t.Fatalf("default_workdir = %q, want %q", got.DefaultWorkdir, project)
	}
	if got.defaultWorkdir() != filepath.Clean(project) {
		t.Fatalf("defaultWorkdir() = %q, want %q", got.defaultWorkdir(), filepath.Clean(project))
	}

	// An unset default_workdir stays valid and falls back to the work root.
	empty := base
	empty.DefaultWorkdir = ""
	writeTestConfig(t, cfgPath, empty)
	got, err = readConfig(cfgPath)
	if err != nil {
		t.Fatalf("empty default_workdir rejected: %v", err)
	}
	if got.defaultWorkdir() != filepath.Clean(root) {
		t.Fatalf("defaultWorkdir() = %q, want the work root %q", got.defaultWorkdir(), filepath.Clean(root))
	}

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"relative path", "project", "absolute"},
		{"outside the work root", outside, "inside work_root"},
		{"missing directory", filepath.Join(root, "missing"), "existing directory"},
		{"regular file", regularFile, "existing directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := base
			bad.DefaultWorkdir = tc.value
			writeTestConfig(t, cfgPath, bad)
			_, err := readConfig(cfgPath)
			if err == nil {
				t.Fatalf("default_workdir %q was accepted", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not explain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDefaultWorkdirPathResolution covers the path resolution rules:
// relative and empty paths anchor at the default working directory, absolute
// in-work-root paths stay reachable, and work_root remains the boundary.
func TestDefaultWorkdirPathResolution(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{WorkRoot: root, DefaultWorkdir: project}

	if got := cfg.defaultWorkdir(); got != filepath.Clean(project) {
		t.Fatalf("defaultWorkdir() = %q, want %q", got, filepath.Clean(project))
	}
	if got := (Config{WorkRoot: root}).defaultWorkdir(); got != filepath.Clean(root) {
		t.Fatalf("defaultWorkdir() without configuration = %q, want %q", got, filepath.Clean(root))
	}
	unclean := Config{WorkRoot: root, DefaultWorkdir: project + "/./sub/.."}
	if got := unclean.defaultWorkdir(); got != filepath.Clean(project) {
		t.Fatalf("defaultWorkdir() did not clean the configured path: %q", got)
	}

	// An empty or blank request lands on the default working directory.
	for _, requested := range []string{"", "   "} {
		got, err := resolveConfigPath(cfg, requested)
		if err != nil {
			t.Fatalf("resolveConfigPath(%q) failed: %v", requested, err)
		}
		if got != filepath.Clean(project) {
			t.Fatalf("resolveConfigPath(%q) = %q, want %q", requested, got, filepath.Clean(project))
		}
	}
	// A relative request resolves against the default working directory.
	got, err := resolveConfigPath(cfg, "notes/todo.md")
	if err != nil {
		t.Fatalf("relative path resolution failed: %v", err)
	}
	if want := filepath.Join(project, "notes", "todo.md"); got != want {
		t.Fatalf("relative path = %q, want %q", got, want)
	}
	// An explicit absolute path inside work_root stays reachable even when it
	// sits outside the default working directory.
	other := filepath.Join(root, "other.md")
	got, err = resolveConfigPath(cfg, other)
	if err != nil {
		t.Fatalf("absolute in-work_root path rejected: %v", err)
	}
	if got != other {
		t.Fatalf("absolute in-work_root path = %q, want %q", got, other)
	}
	// Everything outside work_root stays rejected.
	for _, bad := range []string{
		"../../escape.md",
		"notes/../../../escape.md",
		filepath.Join(filepath.Dir(root), "outside.md"),
		"/etc/passwd",
	} {
		if _, err := resolveConfigPath(cfg, bad); err == nil {
			t.Fatalf("path %q escaped the work root", bad)
		}
	}
	// resolveWorkPath keeps its original direct-call contract.
	got, err = resolveWorkPath(root, "notes/test.md")
	if err != nil || got != filepath.Join(root, "notes", "test.md") {
		t.Fatalf("resolveWorkPath changed behaviour: %q %v", got, err)
	}
	if _, err := resolveWorkPath(root, "../../etc/shadow"); err == nil {
		t.Fatal("resolveWorkPath stopped rejecting work_root escapes")
	}
}

// TestDefaultWorkdirToolResolution exercises the tool surface: relative paths
// and workdirs anchor at the default working directory, explicit in-work_root
// absolute paths keep working, and out-of-work_root targets stay refused.
func TestDefaultWorkdirToolResolution(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "note.txt"), []byte("PROJECT_MARKER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "root-only.txt"), []byte("ROOT_MARKER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := newTestAppWithConfig(t, baseTestConfig(t, root, project))
	server := mcp.NewServer(&mcp.Implementation{Name: "default-workdir", Version: "1"}, nil)
	app.registerCore(server)
	app.registerEnvironmentParity(server)
	client := contractClient(t, server)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s call failed: %v", name, err)
		}
		return res
	}
	assertCwd := func(want string, res *mcp.CallToolResult) {
		t.Helper()
		if res.IsError {
			t.Fatalf("command failed: %s", toolText(res))
		}
		got, _ := contractJSON(t, res.StructuredContent)["output"].(string)
		got = strings.TrimSpace(got)
		resolvedWant, _ := filepath.EvalSymlinks(want)
		resolvedGot, _ := filepath.EvalSymlinks(got)
		if got != want && resolvedGot != resolvedWant {
			t.Fatalf("working directory = %q, want %q", got, want)
		}
	}

	// A relative read resolves against the default working directory.
	res := call("read_file", map[string]any{"path": "note.txt"})
	if res.IsError || !strings.Contains(toolText(res), "PROJECT_MARKER") {
		t.Fatalf("relative read_file did not use the default working directory: %v %s", res.IsError, toolText(res))
	}
	// An explicit absolute path inside work_root stays reachable.
	res = call("read_file", map[string]any{"path": filepath.Join(root, "root-only.txt")})
	if res.IsError || !strings.Contains(toolText(res), "ROOT_MARKER") {
		t.Fatalf("absolute in-work_root read_file failed: %v %s", res.IsError, toolText(res))
	}
	// Targets outside work_root stay refused.
	for _, bad := range []string{
		"/etc/passwd",
		"../../escape.txt",
		filepath.Join(filepath.Dir(root), "outside.txt"),
	} {
		res = call("read_file", map[string]any{"path": bad})
		if !res.IsError {
			t.Fatalf("read_file accepted %q outside work_root", bad)
		}
	}
	// A relative write lands in the default working directory.
	res = call("write_file", map[string]any{"path": "written.txt", "content": "WRITTEN_MARKER\n"})
	if res.IsError {
		t.Fatalf("relative write_file failed: %s", toolText(res))
	}
	body, err := os.ReadFile(filepath.Join(project, "written.txt"))
	if err != nil || !strings.Contains(string(body), "WRITTEN_MARKER") {
		t.Fatalf("relative write_file did not write into the default working directory: %v %s", err, string(body))
	}
	if _, err := os.Stat(filepath.Join(root, "written.txt")); err == nil {
		t.Fatal("relative write_file wrote outside the default working directory")
	}
	// A relative write cannot escape the work root.
	res = call("write_file", map[string]any{"path": "../../escape.txt", "content": "nope\n"})
	if !res.IsError {
		t.Fatal("write_file accepted a path outside work_root")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); err == nil {
		t.Fatal("write_file wrote outside work_root")
	}
	// exec_command without workdir runs in the default working directory.
	res = call("exec_command", map[string]any{"cmd": "pwd", "yield_time_ms": 10000})
	assertCwd(project, res)
	// A relative workdir resolves against the default working directory.
	res = call("exec_command", map[string]any{"cmd": "pwd", "workdir": "sub", "yield_time_ms": 10000})
	assertCwd(filepath.Join(project, "sub"), res)
	// An absolute in-work_root workdir is honoured.
	res = call("exec_command", map[string]any{"cmd": "pwd", "workdir": root, "yield_time_ms": 10000})
	assertCwd(root, res)
	// A workdir outside work_root stays refused.
	res = call("exec_command", map[string]any{"cmd": "pwd", "workdir": "../..", "yield_time_ms": 10000})
	if !res.IsError {
		t.Fatalf("exec_command accepted a workdir outside work_root: %s", toolText(res))
	}
	// get_environment reports the default working directory.
	res = call("get_environment", map[string]any{})
	if res.IsError {
		t.Fatalf("get_environment failed: %s", toolText(res))
	}
	cwd, _ := contractJSON(t, res.StructuredContent)["cwd"].(string)
	if cwd != filepath.Clean(project) {
		t.Fatalf("get_environment cwd = %q, want %q", cwd, filepath.Clean(project))
	}
}

// TestConfigStoreUpdatePreservesDefaultWorkdir proves that a runtime tuning
// update keeps the configured default working directory on disk and in memory.
func TestConfigStoreUpdatePreservesDefaultWorkdir(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, baseTestConfig(t, root, project))
	store, err := newConfigStore(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.Update(256*1024*1024, 192*1024, 90)
	if err != nil {
		t.Fatalf("config update failed: %v", err)
	}
	if updated.DefaultWorkdir != project {
		t.Fatalf("updated config default_workdir = %q, want %q", updated.DefaultWorkdir, project)
	}
	if store.Get().DefaultWorkdir != project {
		t.Fatalf("stored config default_workdir = %q, want %q", store.Get().DefaultWorkdir, project)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"default_workdir": "`+project+`"`) {
		t.Fatalf("persisted config lost default_workdir: %s", string(raw))
	}
	reloaded, err := readConfig(cfgPath)
	if err != nil {
		t.Fatalf("reloaded config rejected: %v", err)
	}
	if reloaded.DefaultWorkdir != project {
		t.Fatalf("reloaded config default_workdir = %q, want %q", reloaded.DefaultWorkdir, project)
	}
	if reloaded.defaultWorkdir() != filepath.Clean(project) {
		t.Fatalf("reloaded defaultWorkdir() = %q, want %q", reloaded.defaultWorkdir(), filepath.Clean(project))
	}
}

// TestAgentInstructionsKeepProjectDocAndSkillCatalog proves that the
// bootstrap instructions keep a 9 KiB AGENTS.md and the full 94-Skill
// catalogue without truncation.
func TestAgentInstructionsKeepProjectDocAndSkillCatalog(t *testing.T) {
	if maxBootstrapInstructionsBytes < 32*1024 {
		t.Fatalf("bootstrap instruction budget = %d, want at least 32 KiB", maxBootstrapInstructionsBytes)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	skillsRoot := filepath.Join(root, "skills")
	names := make([]string, 0, 94)
	for i := 0; i < 94; i++ {
		name := fmt.Sprintf("owner-skill-catalogue-entry-%02d", i)
		names = append(names, name)
		dir := filepath.Join(skillsRoot, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		description := strings.Repeat("Canonical owner skill for one shared capability; ", 3)
		body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const head = "AGENTS_HEAD_MARKER\n"
	const tail = "\nAGENTS_TAIL_MARKER"
	agents := head + strings.Repeat("A", 9*1024-len(head)-len(tail)) + tail
	if len(agents) != 9*1024 {
		t.Fatalf("AGENTS.md fixture is %d bytes, want 9 KiB", len(agents))
	}
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte(agents), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := baseTestConfig(t, root, project)
	cfg.SkillsRoot = skillsRoot
	app := newTestAppWithConfig(t, cfg)
	skills, err := app.listCanonicalSkills("")
	if err != nil {
		t.Fatalf("canonical Skills are not readable: %v", err)
	}
	if len(skills.Skills) != 94 {
		t.Fatalf("canonical Skills = %d, want 94", len(skills.Skills))
	}
	doc, err := app.parityDoc()
	if err != nil {
		t.Fatalf("project doc failed: %v", err)
	}
	if !strings.Contains(doc.Content, "AGENTS_TAIL_MARKER") {
		t.Fatal("project doc does not carry the 9 KiB AGENTS.md")
	}

	instructions := app.buildAgentInstructions()
	if len(instructions) > maxBootstrapInstructionsBytes {
		t.Fatalf("instructions are %d bytes, above the %d byte budget", len(instructions), maxBootstrapInstructionsBytes)
	}
	if !strings.Contains(instructions, "AGENTS_TAIL_MARKER") {
		t.Fatal("AGENTS.md was truncated in the bootstrap instructions")
	}
	if strings.Contains(instructions, "[Project instructions truncated") {
		t.Fatal("bootstrap instructions still report a truncated project document")
	}
	if !strings.Contains(instructions, "- Work root: "+root) {
		t.Fatal("bootstrap instructions do not report the work root")
	}
	if !strings.Contains(instructions, "- Default working directory: "+project) {
		t.Fatal("bootstrap instructions do not report the default working directory")
	}
	for _, name := range names {
		if !strings.Contains(instructions, "- "+name+" (user) — ") {
			t.Fatalf("canonical Skill %q is missing from the bootstrap catalogue", name)
		}
	}
	if strings.Contains(instructions, "[Catalogue preview:") {
		t.Fatal("the 94-Skill catalogue no longer fits the catalogue budget")
	}
	// The fixture must keep reproducing the old truncation, otherwise the
	// regression guard above would stop protecting the real 9 KiB case.
	const marker = "\n## Project instructions (AGENTS.md)\n\n"
	prefix := strings.Index(instructions, marker)
	if prefix < 0 {
		t.Fatal("bootstrap instructions have no project instructions section")
	}
	if prefix+len(marker)+len(doc.Content)+100 <= 24*1024 {
		t.Logf("fixture no longer reproduces the old 24 KiB truncation (prefix=%d project doc=%d bytes)", prefix, len(doc.Content))
	}
}

// TestProjectDocStartsAtDefaultWorkdir proves that project rules are read
// from the configured default working directory, including when the work root
// is the filesystem root.
func TestProjectDocStartsAtDefaultWorkdir(t *testing.T) {
	for _, tc := range []struct {
		name          string
		root          string
		projectInRoot bool
	}{
		{"scoped work root", "", true},
		{"filesystem root", "/", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := tc.root
			project := ""
			if tc.projectInRoot {
				root = t.TempDir()
				project = filepath.Join(root, "project")
			} else {
				project = filepath.Join(t.TempDir(), "project")
			}
			if err := os.MkdirAll(project, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("DEFAULT_SCOPE_MARKER\n"), 0600); err != nil {
				t.Fatal(err)
			}
			app := newTestAppWithConfig(t, baseTestConfig(t, root, project))
			doc, err := app.parityDoc()
			if err != nil {
				t.Fatalf("project doc failed: %v", err)
			}
			if !strings.Contains(doc.Content, "DEFAULT_SCOPE_MARKER") {
				t.Fatalf("project doc did not start at the default working directory: %q", doc.Content)
			}
			server := mcp.NewServer(&mcp.Implementation{Name: "default-workdir-doc", Version: "1"}, nil)
			app.registerParity(server)
			client := contractClient(t, server)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			res, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "get_project_doc", Arguments: map[string]any{}})
			if err != nil {
				t.Fatalf("get_project_doc failed: %v", err)
			}
			if res.IsError || !strings.Contains(toolText(res), "DEFAULT_SCOPE_MARKER") {
				t.Fatalf("get_project_doc did not report the default scope rules: %v %s", res.IsError, toolText(res))
			}
		})
	}
}
