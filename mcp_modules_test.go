package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type moduleFixtureState struct {
	Routes []map[string]any            `json:"routes"`
	Tools  map[string][]map[string]any `json:"tools"`
	Exact  map[string]map[string]any   `json:"exact"`
	Failed map[string]bool             `json:"failed"`
}

type moduleFixture struct {
	app           *App
	state, events string
}

func newModuleFixture(t *testing.T, state moduleFixtureState) *moduleFixture {
	t.Helper()
	dir := t.TempDir()
	f := &moduleFixture{state: filepath.Join(dir, "state.json"), events: filepath.Join(dir, "events.jsonl")}
	f.save(t, state)
	t.Setenv("AMG_MODULE_FIXTURE_STATE", f.state)
	t.Setenv("AMG_MODULE_FIXTURE_EVENTS", f.events)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(dir, "helper")
	// Remove only the race runtime's child exit grace; preserve inherited race options.
	body := "#!/bin/sh\nGORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\" exec '" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' -test.run='^TestMCPModuleHelperProcess$'\n"
	if err := os.WriteFile(wrapper, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	f.app = &App{config: &ConfigStore{cfg: Config{SharedMCPBin: wrapper}}}
	return f
}
func (f *moduleFixture) save(t *testing.T, state moduleFixtureState) {
	t.Helper()
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.state, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *moduleFixture) calls(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(f.events)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var v map[string]any
		if err = json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, v)
	}
	return calls
}
func TestMCPModuleHelperProcess(t *testing.T) {
	path := os.Getenv("AMG_MODULE_FIXTURE_STATE")
	if path == "" {
		return
	}
	load := func() moduleFixtureState {
		b, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var s moduleFixtureState
		if err = json.Unmarshal(b, &s); err != nil {
			panic(err)
		}
		return s
	}
	event := func(method string, in map[string]any) {
		f, err := os.OpenFile(os.Getenv("AMG_MODULE_FIXTURE_EVENTS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		b, _ := json.Marshal(map[string]any{"method": method, "server": in["server"], "tool_name": in["tool_name"]})
		if _, err = f.Write(append(b, '\n')); err != nil {
			panic(err)
		}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "module-helper", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_routes"}, func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		event("routes", in)
		return nil, map[string]any{"routes": load().Routes}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_tools"}, func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		event("tools", in)
		s := load()
		url, _ := in["server"].(string)
		if s.Failed[url] {
			return nil, nil, errors.New("private token=SECRET route failure")
		}
		if name, _ := in["tool_name"].(string); name != "" {
			return nil, map[string]any{"tool": s.Exact[url+"#"+name]}, nil
		}
		return nil, map[string]any{"tools": s.Tools[url], "compact": true}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestMCPModuleSelectedSummaryIsolation(t *testing.T) {
	var targets atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets.Add(1)
		http.Error(w, "unrelated target must not initialize", 503)
	}))
	defer target.Close()
	f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "healthy", "path": "/mcp/healthy", "url": target.URL}, {"name": "broken", "path": "/mcp/broken", "url": target.URL + "/broken"}}, Tools: map[string][]map[string]any{target.URL: {{"name": "z_read", "description": "Read notes"}, {"name": "a_read", "description": "Read files"}}}})
	sources, err := f.app.listSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if targets.Load() != 0 {
		t.Fatal("module discovery initialized targets")
	}
	if strings.Contains(mustContractJSON(t, sources), "toolCount") {
		t.Errorf("unknown count fabricated: %s", mustContractJSON(t, sources))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := f.app.searchRouteTools(ctx, SearchMCPInput{Source: "healthy", Limit: 50})
	if err != nil || len(got.Matches) != 2 {
		t.Errorf("selected empty query did not list helper summaries: out=%+v err=%v", got, err)
	}
	if len(got.Matches) == 2 && got.Matches[0].Name != "a_read" {
		t.Errorf("module tools not ordered by name: %+v", got.Matches)
	}
	filtered, err := f.app.searchRouteTools(ctx, SearchMCPInput{Source: "healthy", Query: "notes"})
	if err != nil || len(filtered.Matches) != 1 || filtered.Matches[0].Name != "z_read" {
		t.Errorf("selected literal query not served by helper: out=%+v err=%v", filtered, err)
	}
	for _, call := range f.calls(t) {
		if call["method"] == "tools" && call["server"] != target.URL {
			t.Errorf("unrelated module listed: %+v", call)
		}
	}
	if targets.Load() != 0 {
		t.Errorf("AMG bypassed canonical helper and initialized HTTP target %d times", targets.Load())
	}
}

func TestMCPModuleFreshRouteAndSchemaChanges(t *testing.T) {
	const oldURL = "http://synthetic.invalid/old"
	const newURL = "http://synthetic.invalid/new"
	state := moduleFixtureState{Routes: []map[string]any{{"name": "kept", "path": "/mcp/kept", "url": oldURL}, {"name": "removed", "path": "/mcp/removed", "url": "http://removed.invalid"}}, Tools: map[string][]map[string]any{oldURL: {{"name": "old_tool", "description": "old directory", "inputSchema": map[string]any{"type": "object", "description": "SCHEMA_NOT_SUMMARY"}}}}}
	f := newModuleFixture(t, state)
	server := mcp.NewServer(&mcp.Implementation{Name: "fresh-modules", Version: "1"}, nil)
	f.app.registerMCPParity(server)
	cs := contractClient(t, server)
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := call("mcp_search_tools", map[string]any{"source": "kept"})
	if first.IsError || !strings.Contains(toolText(first), "old_tool") || strings.Contains(toolText(first), "SCHEMA_NOT_SUMMARY") {
		t.Fatalf("source without query must expose only summaries: %s", toolText(first))
	}
	state.Routes = []map[string]any{{"name": "kept", "path": "/mcp/kept", "url": newURL}, {"name": "added", "path": "/mcp/added", "url": "http://added.invalid", "description": "upstream purpose", "toolCount": 0}}
	state.Tools = map[string][]map[string]any{newURL: {{"name": "new_tool", "description": "new directory"}}}
	state.Exact = map[string]map[string]any{newURL + "#new_tool": {"name": "new_tool", "inputSchema": map[string]any{"type": "object", "description": "NEW_SCHEMA"}}}
	f.save(t, state)
	sources := call("mcp_list_sources", map[string]any{})
	text := toolText(sources)
	if sources.IsError || !strings.Contains(text, "added") || strings.Contains(text, "removed") || !strings.Contains(text, "upstream purpose") || !strings.Contains(text, "\"toolCount\":0") {
		t.Fatalf("same process did not observe canonical metadata: %s", text)
	}
	for _, force := range []bool{false, true} {
		listed := call("mcp_search_tools", map[string]any{"source": "kept", "force_refresh": force})
		if listed.IsError || !strings.Contains(toolText(listed), "new_tool") || strings.Contains(toolText(listed), "old_tool") {
			t.Fatalf("old URL inventory survived: %s", toolText(listed))
		}
	}
	exact := call("mcp_get_tool", map[string]any{"source": "kept", "tool": "new_tool"})
	if exact.IsError || !strings.Contains(toolText(exact), "NEW_SCHEMA") {
		t.Fatalf("fresh exact schema missing: %s", toolText(exact))
	}
	state.Exact[newURL+"#new_tool"]["inputSchema"] = map[string]any{"type": "object", "description": "CHANGED_SCHEMA"}
	f.save(t, state)
	exact = call("mcp_get_tool", map[string]any{"source": "kept", "tool": "new_tool"})
	if exact.IsError || !strings.Contains(toolText(exact), "CHANGED_SCHEMA") {
		t.Fatalf("schema edit retained old exact: %s", toolText(exact))
	}
	if result := call("mcp_search_tools", map[string]any{"source": "removed"}); !result.IsError {
		t.Fatal("removed module returned retained tools")
	}
	state.Tools[newURL] = nil
	f.save(t, state)
	if result := call("mcp_search_tools", map[string]any{"source": "kept"}); result.IsError || strings.Contains(toolText(result), "new_tool") {
		t.Fatal("successful empty directory retained deleted tools")
	}
}

func TestMCPModuleFallbackLiteralPartialAndLimits(t *testing.T) {
	state := moduleFixtureState{Routes: []map[string]any{{"name": "z", "path": "/mcp/z", "url": "http://z.invalid"}, {"name": "b", "path": "/mcp/b", "url": "http://b.invalid"}, {"name": "a", "path": "/mcp/a", "url": "http://a.invalid"}}, Tools: map[string][]map[string]any{
		"http://a.invalid": {{"name": "z_tool", "description": "PATENT lookup"}, {"name": "a_tool", "description": "patent legal status", "inputSchema": map[string]any{"description": "PRIVATE_SCHEMA_ONLY"}}}, "http://z.invalid": {{"name": "lookup", "description": "Patent documents"}}}, Failed: map[string]bool{"http://b.invalid": true}}
	f := newModuleFixture(t, state)
	got, err := f.app.searchRouteTools(context.Background(), SearchMCPInput{Query: "pAtEnT", Limit: 2})
	if err != nil || got.Total != 3 || len(got.Matches) != 2 || got.Matches[0].Name != "a_tool" || got.Matches[1].Name != "z_tool" || !got.Incomplete || got.Stale || !reflect.DeepEqual(got.UnavailableSources, []string{"b"}) {
		t.Fatalf("literal fallback contract failed: %+v %v", got, err)
	}
	if len(got.SourceFailures) != 1 || got.SourceFailures[0].Stage != "tools_list" || got.SourceFailures[0].Kind != "upstream_error" || strings.Contains(mustContractJSON(t, got), "SECRET") || strings.Contains(mustContractJSON(t, got), "http://b.invalid") {
		t.Fatalf("unsafe/missing partial diagnostic: %+v", got)
	}
	for _, match := range got.Matches {
		if match.Score != 1 {
			t.Fatal("compatibility score suggests ranking")
		}
	}
	for _, query := range []string{"专利", "PRIVATE_SCHEMA_ONLY"} {
		out, err := f.app.searchRouteTools(context.Background(), SearchMCPInput{Source: "a", Query: query})
		if err != nil || len(out.Matches) != 0 {
			t.Fatalf("query %q unexpectedly used synonyms/schema: %+v %v", query, out, err)
		}
	}
	for _, input := range []SearchMCPInput{{}, {Source: "a", Limit: -1}, {Source: "a", Limit: 51}} {
		if _, err := f.app.searchRouteTools(context.Background(), input); err == nil {
			t.Errorf("invalid empty/unbounded input accepted: %+v", input)
		}
	}
	state.Failed = map[string]bool{"http://a.invalid": true, "http://b.invalid": true, "http://z.invalid": true}
	f.save(t, state)
	failed, err := f.app.searchRouteTools(context.Background(), SearchMCPInput{Query: "patent"})
	if err != nil || len(failed.Matches) != 0 || !failed.Incomplete || failed.Stale || len(failed.UnavailableSources) != 3 {
		t.Fatalf("all failures returned retained tools: %+v %v", failed, err)
	}
}

func TestMCPModuleFallbackBoundedCancellation(t *testing.T) {
	sources := make([]UpstreamSource, 24)
	for i := range sources {
		sources[i] = UpstreamSource{ID: fmt.Sprintf("%02d", i)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var active, peak atomic.Int32
	started := make(chan struct{}, len(sources))
	fetch := func(ctx context.Context, source UpstreamSource) ([]json.RawMessage, error) {
		if source.ID == "00" {
			return []json.RawMessage{json.RawMessage(`{"name":"ready"}`)}, nil
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	type response struct {
		out SearchMCPOut
		err error
	}
	done := make(chan response, 1)
	go func() { out, err := scanModuleTools(ctx, sources, fetch); done <- response{out, err} }()
	for range 8 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("bounded fallback did not start workers")
		}
	}
	cancel()
	select {
	case got := <-done:
		if got.err != nil || len(got.out.Matches) != 1 || len(got.out.UnavailableSources) != 23 || len(got.out.SourceFailures) != 23 || !got.out.Incomplete || got.out.Stale || peak.Load() != 8 || active.Load() != 0 {
			t.Fatalf("cancellation silently omitted routes/workers: %+v peak=%d active=%d err=%v", got.out, peak.Load(), active.Load(), got.err)
		}
		for _, failure := range got.out.SourceFailures {
			if failure.Kind != "cancelled" {
				t.Fatalf("wrong cancel kind: %+v", failure)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("fallback ignored cancellation")
	}
	if _, err := scanModuleTools(ctx, sources, fetch); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel lost: %v", err)
	}
}

func TestMCPModuleKnownCallNoListAndNativeResult(t *testing.T) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "known-call", Version: "1"}, nil)
	var listCalls atomic.Int32
	upstream.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				listCalls.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	want := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "native text"}, &mcp.ImageContent{Data: []byte{1, 2}, MIMEType: "image/png"}, &mcp.ResourceLink{URI: "fixture://artifact", Name: "artifact"}}, StructuredContent: map[string]any{"native": true}, Meta: mcp.Meta{"fixture": "keep"}, IsError: true}
	upstream.AddTool(&mcp.Tool{Name: "known", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return want, nil })
	web := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer web.Close()
	f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "fixture", "path": "/mcp/fixture", "url": web.URL}}})
	outer := mcp.NewServer(&mcp.Implementation{Name: "known-outer", Version: "1"}, nil)
	f.app.registerMCPParity(outer)
	outer.AddReceivingMiddleware(toolContractsMiddleware)
	got, err := contractClient(t, outer).CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_call_tool", Arguments: map[string]any{"source": "fixture", "tool": "known"}})
	if err != nil || got == nil {
		t.Fatalf("known call: %v", err)
	}
	if mustContractJSON(t, got.Content) != mustContractJSON(t, want.Content) || mustContractJSON(t, got.StructuredContent) != mustContractJSON(t, want.StructuredContent) || got.Meta["fixture"] != "keep" || got.IsError != want.IsError {
		t.Fatalf("native result altered: %s", mustContractJSON(t, got))
	}
	if listCalls.Load() != 0 {
		t.Fatal("SDK call performed tools/list preflight")
	}
	for _, call := range f.calls(t) {
		if call["method"] == "tools" {
			t.Fatalf("known call listed helper directory: %+v", call)
		}
	}
}

func TestMCPModuleUpstreamContextIsolation(t *testing.T) {
	type outerKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), outerKey{}, "outer-protocol"))
	defer cancel()
	ctx, stop := upstreamContext(parent, time.Second)
	defer stop()
	if ctx.Value(outerKey{}) != nil {
		t.Fatal("inbound protocol value leaked")
	}
	cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("parent cancellation lost")
	}
}

func TestMCPModuleCompactExactIdentity(t *testing.T) {
	const url = "http://synthetic.invalid/mcp"
	f := newModuleFixture(t, moduleFixtureState{Routes: []map[string]any{{"name": "fixture", "path": "/mcp/fixture", "url": url}}, Tools: map[string][]map[string]any{url: {{"name": "exact_tool", "description": "summary"}}}, Exact: map[string]map[string]any{
		url + "#exact_tool": {"name": "exact_tool", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}, "annotations": map[string]any{"readOnlyHint": true}, "_meta": map[string]any{"fixture": "keep"}},
		url + "#wrong":      {"name": "other", "inputSchema": map[string]any{"type": "object"}}, url + "#summary": {"name": "summary", "description": "no schema"}}})
	server := mcp.NewServer(&mcp.Implementation{Name: "module-contract", Version: "1"}, nil)
	f.app.registerMCPParity(server)
	cs := contractClient(t, server)
	summary, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_search_tools", Arguments: map[string]any{"source": "fixture"}})
	if err != nil || summary.IsError || !strings.Contains(toolText(summary), "exact_tool") || strings.Contains(toolText(summary), "inputSchema") {
		t.Fatalf("compact summary before exact: %+v %v", summary, err)
	}
	for _, name := range []string{"exact_tool", "wrong", "summary"} {
		result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{"source": "fixture", "tool": name}})
		if err != nil {
			t.Fatal(err)
		}
		if name != "exact_tool" {
			if !result.IsError {
				t.Errorf("wrong name/missing schema accepted: %s", name)
			}
			continue
		}
		if result.IsError || !strings.Contains(toolText(result), "readOnlyHint") || !strings.Contains(toolText(result), "keep") {
			t.Errorf("exact metadata lost: %+v", result)
		}
	}
}
