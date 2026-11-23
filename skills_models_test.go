package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// skillsListSession drives skills_list through the real MCP stack so the
// TextContent and structuredContent the SDK produces are both observable.
func skillsListSession(t *testing.T, app *App) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "skills-list-test", Version: "1"}, nil)
	app.registerCore(server)
	c, sv := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), sv)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "skills-list-client", Version: "1"}, nil).
		Connect(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func writeSkillFixture(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n# " + name + "\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestSkillsListTextIsSingleCompactSummary pins the wire contract fixed for
// v1.2.4: the TextContent block carries the compact "name — description"
// summary once, while structuredContent still carries both the skills array
// and the content field. Returning a nil result let go-sdk v1.8.0 serialize the
// whole SkillListOut into the text block, duplicating every entry.
func TestSkillsListTextIsSingleCompactSummary(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().SkillsRoot
	writeSkillFixture(t, root, "alpha-skill", "First fixture skill")
	writeSkillFixture(t, root, "beta-skill", "Second fixture skill")
	writeSkillFixture(t, root, "gamma-skill", "Third fixture skill")
	session := skillsListSession(t, app)

	check := func(t *testing.T, args map[string]any, wantSkills int, absent []string) {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_list", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("skills_list failed: err=%v res=%#v", err, res)
		}
		text := toolText(res)
		if strings.TrimSpace(text) == "" {
			t.Fatal("skills_list TextContent is empty")
		}
		if strings.HasPrefix(strings.TrimSpace(text), "{") {
			t.Fatalf("skills_list TextContent is the serialized structured output again: %q", text)
		}
		if strings.Contains(text, "\"skills\"") || strings.Contains(text, "\"content\"") {
			t.Fatalf("skills_list TextContent repeats the JSON envelope: %q", text)
		}
		structured, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if len(text)*2 >= len(structured) {
			t.Fatalf("text is %d bytes and structured is %d bytes; the duplication was not removed", len(text), len(structured))
		}
		var out SkillListOut
		encoded, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatalf("structuredContent does not decode into SkillListOut: %v (%s)", err, encoded)
		}
		if len(out.Skills) != wantSkills {
			t.Fatalf("structuredContent skills = %d, want %d", len(out.Skills), wantSkills)
		}
		if out.Content != text {
			t.Fatalf("structuredContent content does not match the text block: %q vs %q", out.Content, text)
		}
		for _, name := range absent {
			if strings.Contains(text, name) {
				t.Fatalf("filtered text still mentions %q: %q", name, text)
			}
		}
	}

	t.Run("full_catalog", func(t *testing.T) {
		check(t, map[string]any{}, 3, nil)
	})
	t.Run("search_filter", func(t *testing.T) {
		check(t, map[string]any{"search": "beta"}, 1, []string{"alpha-skill", "gamma-skill"})
	})
	t.Run("empty_catalog", func(t *testing.T) {
		emptyApp, _, _ := testApp(t)
		if err := os.MkdirAll(emptyApp.config.Get().SkillsRoot, 0700); err != nil {
			t.Fatal(err)
		}
		emptySession := skillsListSession(t, emptyApp)
		res, err := emptySession.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_list", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("empty skills_list failed: err=%v res=%#v", err, res)
		}
		text := toolText(res)
		if strings.TrimSpace(text) == "" {
			t.Fatal("empty catalogue must still emit a readable hint, not an empty TextContent")
		}
		if !strings.Contains(text, "No Skills found") {
			t.Fatalf("empty catalogue hint is not readable: %q", text)
		}
		res, err = emptySession.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_list", Arguments: map[string]any{"search": "nothing"}})
		if err != nil || res.IsError {
			t.Fatalf("unmatched search failed: err=%v res=%#v", err, res)
		}
		if text := toolText(res); !strings.Contains(text, "No Skills match") {
			t.Fatalf("unmatched search hint is not readable: %q", text)
		}
	})
}

// TestCanonicalSkillsRequireConfiguredRoot pins the operator-facing error for
// an unset skills_root: a plain message instead of an os.ReadDir errno.
func TestCanonicalSkillsRequireConfiguredRoot(t *testing.T) {
	app := &App{config: &ConfigStore{cfg: Config{SkillsRoot: ""}}}
	if _, err := app.listCanonicalSkills(""); err == nil || err.Error() != "skills_root not configured" {
		t.Fatalf("listCanonicalSkills error = %v, want skills_root not configured", err)
	}
	if _, err := app.readCanonicalSkill(SkillReadInput{Name: "demo"}); err == nil || err.Error() != "skills_root not configured" {
		t.Fatalf("readCanonicalSkill error = %v, want skills_root not configured", err)
	}
}

// TestSkillsListTextSizeAgainstConfiguredRoot measures the wire size of one
// skills_list text block against a real Skills root. It is a measurement aid,
// so it only runs when AMG_MEASURE_SKILLS_ROOT names an existing directory.
// The old behavior can be compared directly: the pre-v1.2.4 text block was the
// whole serialized SkillListOut, whose size is the structuredContent size.
func TestSkillsListTextSizeAgainstConfiguredRoot(t *testing.T) {
	root := os.Getenv("AMG_MEASURE_SKILLS_ROOT")
	if root == "" {
		t.Skip("set AMG_MEASURE_SKILLS_ROOT to measure a real Skills root")
	}
	app := &App{config: &ConfigStore{cfg: Config{SkillsRoot: root, MaxOutputBytes: 512 * 1024}}}
	session := skillsListSession(t, app)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "skills_list", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("skills_list failed: err=%v res=%#v", err, res)
	}
	text := toolText(res)
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out SkillListOut
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatal(err)
	}
	t.Logf("in_memory: skills=%d text_bytes=%d structured_bytes=%d ratio=%.2f", len(out.Skills), len(text), len(structured), float64(len(text))/float64(len(structured)))
	if len(text) >= len(structured) {
		t.Fatalf("text (%d B) is not smaller than the full structured payload (%d B)", len(text), len(structured))
	}

	// Cross-check the same call through the production handler stack, where the
	// contracts middleware also runs, so the measured size is what a client
	// actually receives.
	dir := t.TempDir()
	token := strings.Repeat("m", 48)
	tokPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Listen: "127.0.0.1:3100", WorkRoot: dir, StateDir: filepath.Join(dir, "state"),
		SkillsRoot: root, TokenFile: tokPath, PublicURL: "",
		MaxFileBytes: 1 << 20, MaxOutputBytes: 512 * 1024, CommandTimeoutSec: 30,
	}
	encodedCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, encodedCfg, 0600); err != nil {
		t.Fatal(err)
	}
	liveApp, err := newApp(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(liveApp.handler())
	defer web.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"skills_list","arguments":{}}}`
	request, err := http.NewRequest(http.MethodPost, web.URL+"/mcp/stateless", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "127.0.0.1:3100"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := web.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stateless skills_list status %d: %s", response.StatusCode, payload)
	}
	var parsed struct {
		Result struct {
			Content           []map[string]any `json:"content"`
			StructuredContent map[string]any   `json:"structuredContent"`
			IsError           bool             `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("decode stateless response: %v", err)
	}
	if parsed.Result.IsError {
		t.Fatalf("stateless skills_list returned isError: %s", payload)
	}
	if len(parsed.Result.Content) != 1 {
		t.Fatalf("stateless skills_list returned %d content blocks, want 1", len(parsed.Result.Content))
	}
	textBlock, _ := parsed.Result.Content[0]["text"].(string)
	structuredBlock, err := json.Marshal(parsed.Result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("over_http: text_bytes=%d structured_bytes=%d ratio=%.2f", len(textBlock), len(structuredBlock), float64(len(textBlock))/float64(len(structuredBlock)))
	if len(textBlock) >= len(structuredBlock) {
		t.Fatalf("HTTP text block (%d B) is not smaller than structuredContent (%d B)", len(textBlock), len(structuredBlock))
	}
	if strings.HasPrefix(strings.TrimSpace(textBlock), "{") {
		t.Fatalf("HTTP text block is the serialized structured payload: %q", textBlock[:min(len(textBlock), 200)])
	}
}
