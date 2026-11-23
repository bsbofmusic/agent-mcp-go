package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Regression for the mechanism in CodexFree v1.9.1:
// server.rs::get_info -> instructions.rs::build_initial_instructions.
// It must appear in the actual initialize response, not just a callable tool.
func TestCodexFreeStyleInitializationBrief(t *testing.T) {
	a, _, token := testApp(t)
	cfg := a.config.Get()
	skillDir := filepath.Join(cfg.SkillsRoot, "owner-one")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: owner-one\ndescription: Operate the test owner's service safely\n---\n# Owner instructions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkRoot, "AGENTS.md"), []byte("PROJECT_MARKER_0234: Prefer reading the existing repository before edits."), 0600); err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"amg-instruction-regression","version":"1"}}}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3100/mcp", strings.NewReader(body))
	request.Host = "127.0.0.1:3100"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status %d: %s", response.Code, response.Body.String())
	}
	var got struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	instructions := got.Result.Instructions
	if instructions == "" {
		t.Fatal("initialize response lacks agent instructions")
	}
	ordered := []string{"You are operating a real", "## Environment", "## Available Skills", "owner-one", "## Project instructions", "PROJECT_MARKER_0234"}
	pos := -1
	for _, item := range ordered {
		next := strings.Index(instructions, item)
		if next <= pos {
			t.Fatalf("instructions misplaced or missing %q after index %d", item, pos)
		}
		pos = next
	}
	if !strings.Contains(instructions, "skills_read") || !strings.Contains(instructions, "mcp_search_tools") || strings.Contains(instructions, "MemOS") {
		t.Fatalf("missing native tool-routing guidance")
	}
	// The brief travels to arbitrary deployments, so it must not hardcode one
	// operator's private archive layout or OS account name.
	for _, private := range []string{"agent OS user", "Obsidian brain/", "raw/transcripts/agentsview/"} {
		if strings.Contains(instructions, private) {
			t.Fatalf("initialization brief hardcodes deployment-private wording %q", private)
		}
	}
	if !utf8.ValidString(instructions) {
		t.Fatalf("instructions exceeded bound or contain invalid UTF-8: %d bytes", len(instructions))
	}
}

// Out-of-the-box regression: a deployment that serves no Skills and no shared
// MCP must still get clear initialization instructions. An empty skills_root is
// a configuration choice, so it must not read as a fault, must not invite a
// retry loop, and must not hardcode one operator's private archive layout.
// The file, shell and Git tools must keep working in that state.
func TestUnconfiguredSkillsAndMCPStayClearlyUsable(t *testing.T) {
	root := t.TempDir()
	cfg := baseTestConfig(t, root, "")
	cfg.SkillsRoot = ""
	cfg.SharedMCPBin = ""
	app := newTestAppWithConfig(t, cfg)

	compact := app.buildInitializeInstructions()
	full := app.buildAgentInstructions()
	for name, text := range map[string]string{"initialize": compact, "brief": full} {
		if !strings.Contains(text, "Skills integration is not configured") {
			t.Fatalf("%s instructions do not state that Skills are unconfigured", name)
		}
		if !strings.Contains(text, "file, shell and Git tools work without it") {
			t.Fatalf("%s instructions do not say which tools remain usable", name)
		}
		for _, forbidden := range []string{
			"currently unavailable",
			"use skills_list to retry",
			"agent OS user",
			"Obsidian brain/",
			"raw/transcripts/agentsview/",
		} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s instructions contain %q for an unconfigured deployment", name, forbidden)
			}
		}
		// The compact-index note advertises skills_list, which cannot succeed
		// here; it must not appear without a catalogue to back it.
		if strings.Contains(text, "This is a compact index") {
			t.Fatalf("%s instructions advertise a Skills index that is not served", name)
		}
		if !utf8.ValidString(text) {
			t.Fatalf("%s instructions contain invalid UTF-8", name)
		}
	}
	if !strings.Contains(compact, "## Environment") || !strings.Contains(compact, "## Available Skills") {
		t.Fatalf("unconfigured initialize instructions lost a fixed section")
	}

	// The real initialize handshake must carry the same clear text.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"unconfigured-smoke","version":"1"}}}`
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3100/mcp", strings.NewReader(body))
	request.Host = "127.0.0.1:3100"
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(mustReadToken(t, cfg.TokenFile)))
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status %d: %s", response.Code, response.Body.String())
	}
	var handshake struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &handshake); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(handshake.Result.Instructions, "Skills integration is not configured") {
		t.Fatalf("initialize handshake does not carry the unconfigured Skills notice")
	}
	if strings.Contains(handshake.Result.Instructions, "currently unavailable") {
		t.Fatalf("initialize handshake reports an intentional configuration as a fault")
	}

	// Core capabilities must initialize and run while Skills are absent.
	ctx := t.Context()
	server := mcp.NewServer(&mcp.Implementation{Name: "unconfigured-core", Version: "1"},
		&mcp.ServerOptions{Instructions: app.buildAgentInstructions()})
	app.registerCore(server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go server.Run(ctx, serverTransport)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "unconfigured-client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport: %v", name, err)
		}
		return toolText(res), res.IsError
	}

	marker := filepath.Join(root, "readme.txt")
	if err := os.WriteFile(marker, []byte("UNCONFIGURED_MARKER\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, bad := call("read_file", map[string]any{"path": marker}); bad || !strings.Contains(out, "UNCONFIGURED_MARKER") {
		t.Fatalf("read_file failed without Skills configured: isError=%v out=%q", bad, out)
	}
	if out, bad := call("list_directory", map[string]any{"path": root}); bad || !strings.Contains(out, "readme.txt") {
		t.Fatalf("list_directory failed without Skills configured: isError=%v out=%q", bad, out)
	}
	if out, bad := call("exec_command", map[string]any{"cmd": "printf shell_ok", "workdir": root}); bad || !strings.Contains(out, "shell_ok") {
		t.Fatalf("exec_command failed without Skills configured: isError=%v out=%q", bad, out)
	}

	repo := filepath.Join(root, "repo")
	git := exec.Command("git", "init", "-q", repo)
	git.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Setenv("LC_ALL", "C")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	if out, bad := call("git_status", map[string]any{"directory": repo}); bad || !strings.Contains(strings.ToLower(out), "nothing to commit") {
		t.Fatalf("git_status failed without Skills configured: isError=%v out=%q", bad, out)
	}
}

// mustReadToken returns the configured bearer token for a test App.
func mustReadToken(t *testing.T, path string) string {
	t.Helper()
	token, err := readBearerToken(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(token)
}

func TestNativeAgentBriefRefreshesSkillCatalogAndPreservesDescriptions(t *testing.T) {
	a, _, _ := testApp(t)
	cfg := a.config.Get()
	if err := os.MkdirAll(cfg.SkillsRoot, 0700); err != nil {
		t.Fatal(err)
	}
	first := a.buildAgentInstructions()
	if !strings.Contains(first, "## Available Skills") {
		t.Fatalf("missing skills section")
	}
	if !strings.Contains(first, "use skills_list") {
		t.Fatalf("missing fallback discovery guidance")
	}
	if strings.Contains(first, "currently unavailable") {
		t.Fatalf("an empty Skills directory must not be described as a fault")
	}
	// No restart or second registry is required for get_agent_brief to notice
	// canonical Skills Manager additions.
	skill := filepath.Join(cfg.SkillsRoot, "dynamic-owner", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: dynamic-owner\ndescription: "+strings.Repeat("长", 300)+"\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	second := a.buildAgentInstructions()
	if strings.Contains(first, "dynamic-owner") || !strings.Contains(second, "dynamic-owner") {
		t.Fatalf("catalogue failed to reflect on-disk Skill change")
	}
	if !strings.Contains(second, strings.Repeat("长", 300)) {
		t.Fatalf("large Skill description was truncated")
	}
	if !utf8.ValidString(second) {
		t.Fatalf("invalid or oversized Skill guidance")
	}
	if strings.Contains(second, "SKILL.md body:") {
		t.Fatalf("skill bodies must remain on-demand")
	}
}

func TestNewConnectionsRefreshInitializationInstructions(t *testing.T) {
	a, _, token := testApp(t)
	handler := a.handler()
	cfg := a.config.Get()
	initialize := func(path string) string {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fresh-client","version":"1"}}}`
		discover := strings.HasSuffix(path, "#discover")
		path = strings.TrimSuffix(path, "#discover")
		if discover {
			body = `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fresh-client","version":"1"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
		}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:3100"+path, strings.NewReader(body))
		if discover {
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "server/discover")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var response struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || response.Result.Instructions == "" {
			t.Fatalf("initialize failed (%s): %d %s", path, rec.Code, rec.Body.String())
		}
		return response.Result.Instructions
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		if strings.Contains(initialize(path), "fresh-owner-marker") {
			t.Fatal("unexpected fixture marker")
		}
	}
	skill := filepath.Join(cfg.SkillsRoot, "fresh-owner-marker", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: fresh-owner-marker\ndescription: Fresh owner metadata\n---\nPRIVATE_BODY_NOT_BOOTSTRAP"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.WorkRoot, "AGENTS.md"), []byte("FRESH_PROJECT_MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		fresh := initialize(path)
		if !containsAll(fresh, "fresh-owner-marker", "FRESH_PROJECT_MARKER") {
			t.Errorf("new %s connection received stale instructions", path)
		}
		if strings.Contains(fresh, "PRIVATE_BODY_NOT_BOOTSTRAP") {
			t.Error("bootstrap eagerly loaded Skill body")
		}
	}
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/mcp", "/mcp/stateless", "/mcp/stateless#discover"} {
		if strings.Contains(initialize(path), "fresh-owner-marker") {
			t.Errorf("removed owner remains in %s initialization", path)
		}
	}
}

// bootstrap_files regression: configured context files must be inlined into the
// initialize instructions, must follow a hot settings update with no restart or
// reconnect, and must degrade to a read_file pointer instead of blowing the
// shared max_output_bytes text budget.
//
// The section label asserted here is the one agent_instructions.go actually
// emits (appendBootstrapFiles writes "\n\n## Bootstrap context\n").
func TestBootstrapFilesInlineAndTruncation(t *testing.T) {
	a, _, _ := testApp(t)
	cfg := a.config.Get()
	first := filepath.Join(cfg.WorkRoot, "bootstrap1.md")
	second := filepath.Join(cfg.WorkRoot, "bootstrap2.md")
	if err := os.WriteFile(first, []byte("BOOTSTRAP_ONE_MARKER: prefer the existing checkout before edits."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("BOOTSTRAP_TWO_MARKER: run the focused package test first."), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := a.config.Update(SettingsInput{BootstrapFiles: &[]string{first}}); err != nil {
		t.Fatal(err)
	}
	out := a.buildAgentInstructions()
	if !strings.Contains(out, "## Bootstrap context") {
		t.Fatalf("bootstrap section missing from agent instructions")
	}
	if !strings.Contains(out, "BOOTSTRAP_ONE_MARKER") {
		t.Fatal("configured bootstrap file was not inlined")
	}
	if strings.Contains(out, "BOOTSTRAP_TWO_MARKER") {
		t.Fatal("unconfigured file was inlined")
	}

	// Hot update: the next brief reflects the new list without a restart.
	if _, err := a.config.Update(SettingsInput{BootstrapFiles: &[]string{first, second}}); err != nil {
		t.Fatal(err)
	}
	out = a.buildAgentInstructions()
	if !strings.Contains(out, "## Bootstrap context") || !containsAll(out, "BOOTSTRAP_ONE_MARKER", "BOOTSTRAP_TWO_MARKER") {
		t.Fatal("hot-updated bootstrap_files did not reach the instructions")
	}
	if got := len(a.config.Get().BootstrapFiles); got != 2 {
		t.Fatalf("live config holds %d bootstrap files, want 2", got)
	}
	if _, err := a.config.Update(SettingsInput{BootstrapFiles: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.buildAgentInstructions(), "## Bootstrap context") {
		t.Fatal("cleared bootstrap_files still inlined context")
	}

	// A file past the inlining budget is cut, not silently swallowed.
	big := filepath.Join(cfg.WorkRoot, "bootstrap-big.md")
	body := strings.Repeat("oversized bootstrap line\n", 512) + "BOOTSTRAP_TAIL_AFTER_BUDGET"
	if err := os.WriteFile(big, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.config.Update(SettingsInput{MaxOutputBytes: testPointer(1024), BootstrapFiles: &[]string{big}}); err != nil {
		t.Fatal(err)
	}
	if len(body) <= a.config.Get().MaxOutputBytes {
		t.Fatalf("fixture too small to exercise truncation: %d bytes", len(body))
	}
	out = a.buildAgentInstructions()
	if !strings.Contains(out, "[Bootstrap file truncated; call read_file for full content.]") {
		t.Fatal("oversized bootstrap file was not truncated")
	}
	if strings.Contains(out, "BOOTSTRAP_TAIL_AFTER_BUDGET") {
		t.Fatal("truncated bootstrap file leaked content past the budget")
	}
	idx := strings.Index(out, "\n\n## Bootstrap context\n")
	if idx < 0 {
		t.Fatal("bootstrap section heading missing")
	}
	section := out[idx:]
	if len(section) > 1024 {
		t.Fatalf("truncated bootstrap section uses %d bytes, want at most 1024", len(section))
	}
}

func TestBootstrapSectionBudgetWithEmptyAndUnavailableFiles(t *testing.T) {
	a, _, _ := testApp(t)
	root := a.config.Get().WorkRoot
	empty := filepath.Join(root, "empty.md")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing.md")
	for _, tc := range []struct {
		name, path string
	}{
		{"empty", empty},
		{"unavailable", missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := make([]string, 500)
			for i := range files {
				files[i] = tc.path
			}
			const budget = 1024
			if _, err := a.config.Update(SettingsInput{MaxOutputBytes: testPointer(budget), BootstrapFiles: &files}); err != nil {
				t.Fatal(err)
			}
			out := a.buildAgentInstructions()
			idx := strings.Index(out, "\n\n## Bootstrap context\n")
			if idx < 0 {
				t.Fatal("bootstrap section heading missing")
			}
			section := out[idx:]
			if !strings.Contains(section, "### "+tc.path+"\n") {
				t.Fatal("bootstrap file heading missing")
			}
			if tc.name == "unavailable" && !strings.Contains(strings.ToLower(section), "unavailable:") {
				t.Error("missing file did not receive an unavailable notice")
			}
			if len(section) > budget {
				t.Errorf("bootstrap section uses %d bytes, want at most %d", len(section), budget)
			}
		})
	}
}

func TestBootstrapFilesTotalSectionBudgetCap(t *testing.T) {
	const budget = 1024
	for _, tc := range []struct {
		name    string
		repeats int
	}{
		{"raw bytes exceed budget", 600},
		{"only normalized bytes exceed budget", 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := testApp(t)
			path := filepath.Join(a.config.Get().WorkRoot, "invalid-utf8.md")
			if err := os.WriteFile(path, bytes.Repeat([]byte{0xff, 'a'}, tc.repeats), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.config.Update(SettingsInput{MaxOutputBytes: testPointer(budget), BootstrapFiles: &[]string{path}}); err != nil {
				t.Fatal(err)
			}
			out := a.buildAgentInstructions()
			idx := strings.Index(out, "\n\n## Bootstrap context\n")
			if idx < 0 {
				t.Fatal("bootstrap section heading missing")
			}
			section := out[idx:]
			if !utf8.ValidString(section) || len(section) > budget {
				t.Errorf("rendered bootstrap section: %d bytes, valid UTF-8 = %t; want valid UTF-8 within %d bytes", len(section), utf8.ValidString(section), budget)
			}
			encoded, err := json.Marshal(section)
			if err != nil {
				t.Fatal(err)
			}
			var decoded string
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(decoded) || len(decoded) > budget {
				t.Errorf("round-tripped bootstrap section: %d bytes, valid UTF-8 = %t; want valid UTF-8 within %d bytes", len(decoded), utf8.ValidString(decoded), budget)
			}
			if !containsAll(section, "\uFFFDa", "[Bootstrap file truncated; call read_file for full content.]") {
				t.Error("bootstrap section lacks normalized file content or truncation notice")
			}
		})
	}
}

func TestBootstrapAgentBriefNormalizesWithinSectionBudget(t *testing.T) {
	a, srv, token := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, a, srv, token, "/mcp")
	path := filepath.Join(a.config.Get().WorkRoot, "invalid-utf8.md")
	const budget = 1024
	const heading = "\n\n## Bootstrap context\n"
	const savedHeading = "## Saved state (current task only)\n\n"
	handoverOK(t, ctx, client, "settings_update", map[string]any{"max_output_bytes": budget, "bootstrap_files": []string{path}}, "")
	for _, tc := range []struct {
		name      string
		raw       string
		truncated bool
	}{
		{"fits after normalization", "\xff\xff\xff界A\xe2\x82", false},
		{"consecutive invalid bytes exceed budget", strings.Repeat("\xff", budget/2) + "BOOTSTRAP_TAIL", true},
		{"unclosed code fence stays at EOF", "```md\n" + strings.Repeat("x", budget), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			brief := handoverOK(t, ctx, client, "get_agent_brief", map[string]any{"scope": "AMG-BOOTSTRAP-FRESH"}, "")
			beforeHeading, afterHeading, ok := strings.Cut(brief, heading)
			if !ok {
				t.Fatal("bootstrap section heading missing")
			}
			if !strings.Contains(brief, savedHeading+"No saved plan or notes exist for this task scope.") {
				t.Error("fresh scope message does not start in its own saved-state section")
			}
			savedIndex := strings.Index(brief, "## Saved state (current task only)")
			if savedIndex < 0 || savedIndex >= strings.Index(brief, "## Available Skills") || savedIndex >= len(beforeHeading) {
				t.Error("saved state must precede Available Skills and Bootstrap context")
			}
			section := heading + afterHeading
			if !utf8.ValidString(section) || len(section) > budget {
				t.Errorf("bootstrap section through EOF: %d bytes, valid UTF-8 = %t; want valid UTF-8 within %d bytes", len(section), utf8.ValidString(section), budget)
			}
			if tc.truncated && !strings.HasSuffix(afterHeading, "\n[Bootstrap file truncated; call read_file for full content.]\n") {
				t.Error("truncated bootstrap must end the brief with its truncation notice")
			}
			switch {
			case !tc.truncated:
				if !strings.HasPrefix(afterHeading, "\n### "+path+"\n\uFFFD\uFFFD\uFFFD界A\uFFFD\uFFFD\n") {
					t.Errorf("bootstrap file did not normalize each invalid byte: %q", afterHeading)
				}
			case strings.HasPrefix(tc.raw, "```md\n"):
				if !strings.HasPrefix(afterHeading, "\n### "+path+"\n```md\n") || strings.Count(afterHeading, "```") != 1 {
					t.Error("bootstrap section must retain the unclosed code fence at EOF")
				}
			default:
				if !strings.Contains(afterHeading, "\uFFFD\uFFFD\uFFFD") || strings.Contains(afterHeading, "BOOTSTRAP_TAIL") {
					t.Error("oversized invalid run did not produce a normalized, truncated body")
				}
			}
		})
	}
}

func TestBootstrapAgentBriefRefreshesAfterMCPSettingsUpdate(t *testing.T) {
	a, srv, token := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := handoverHTTPClient(t, ctx, a, srv, token, "/mcp")
	oldPath := filepath.Join(a.config.Get().WorkRoot, "old.md")
	newPath := filepath.Join(a.config.Get().WorkRoot, "new.md")
	for path, marker := range map[string]string{oldPath: "OLD_BOOTSTRAP_MARKER", newPath: "NEW_BOOTSTRAP_MARKER"} {
		if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path, want, absent string
	}{
		{oldPath, "OLD_BOOTSTRAP_MARKER", "NEW_BOOTSTRAP_MARKER"},
		{newPath, "NEW_BOOTSTRAP_MARKER", "OLD_BOOTSTRAP_MARKER"},
	} {
		handoverOK(t, ctx, client, "settings_update", map[string]any{"bootstrap_files": []string{tc.path}}, "")
		brief := handoverOK(t, ctx, client, "get_agent_brief", map[string]any{}, "")
		if !strings.Contains(brief, tc.want) || strings.Contains(brief, tc.absent) {
			t.Fatalf("brief after settings_update should include %q and omit %q", tc.want, tc.absent)
		}
	}
}

func TestLiveBootstrap(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LIVE") != "1" {
		t.Skip("live endpoint smoke only")
	}
	c, err := liveTestConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readBearerToken(c.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + c.Listen + "/mcp"
	if remote := os.Getenv("AGENT_MCP_GO_ENDPOINT"); remote != "" {
		endpoint = remote
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "amg-bootstrapping-smoke", Version: "1"}, nil)
	conn, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	instructions := conn.InitializeResult().Instructions
	for _, required := range []string{"## Environment", "## Available Skills"} {
		if !strings.Contains(instructions, required) {
			t.Errorf("server initialize lacks %q", required)
		}
	}
	if !utf8.ValidString(instructions) {
		t.Fatalf("server initialize instructions oversized or invalid UTF-8 (%d bytes)", len(instructions))
	}
	if strings.Contains(instructions, string(token)) {
		t.Fatal("server initialize leaked Bearer token")
	}
}
