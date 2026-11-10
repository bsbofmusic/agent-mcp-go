package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This is a new 1.0.1 benchmark; the original user ten-query text was not retained.
// Expected sources are test labels only, never inputs to production ranking.
var mcpDiscoveryQueries = []struct{ kind, query, source string }{
	{"new10", "查找我之前保存的记忆", "memos"},
	{"new10", "搜索美国外观专利", "patent"},
	{"new10", "查找知识库里的笔记", "obsidian"},
	{"new10", "查找之前的会话历史记录", "sessions"},
	{"new10", "搜索Reddit社区讨论", "reddit"},
	{"new10", "查看电商店铺信息", "shopify"},
	{"new10", "综合搜索互联网资料", "anysearch"},
	{"new10", "管理Cloudflare域名DNS", "cloudflare"},
	{"new10", "七政四余排盘", "tianguan"},
	{"new10", "搜索亚马逊商品详情", "xydc"},
	{"holdout", "回忆以前保存过的偏好", "memos"},
	{"holdout", "查专利法律状态", "patent"},
	{"holdout", "读取本地笔记内容", "obsidian"},
	{"holdout", "检索聊天记录原文", "sessions"},
	{"holdout", "在Reddit找用户帖子", "reddit"},
	{"holdout", "独立站商店销售数据", "shopify"},
	{"holdout", "多引擎联网检索", "anysearch"},
	{"holdout", "Cloudflare DNS records", "cloudflare"},
	{"holdout", "计算七政四余星盘", "tianguan"},
	{"holdout", "Amazon产品评论", "xydc"},
	{"english", "patent legal status", "patent"},
	{"english", "search memory", "memos"},
	{"english", "read note content", "obsidian"},
	{"english", "conversation session history", "sessions"},
	{"mixed", "patent专利法律状态", "patent"},
	{"mixed", "memory记忆检索", "memos"},
	{"mixed", "Reddit社区帖子", "reddit"},
	{"mixed", "shopify商店产品", "shopify"},
	{"ambiguous", "搜索", ""},
	{"ambiguous", "历史", ""},
	{"unknown", "zzzxqvunknowncapability", ""},
	{"single", "的", ""},
	{"single", "我", ""},
	{"single", "查", ""},
}

func sortedDiscoveryMatches(entries []catalogEntry, query string) []SearchMatch {
	matches := rankMCPEntries(entries, query)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		if matches[i].SourceId != matches[j].SourceId {
			return matches[i].SourceId < matches[j].SourceId
		}
		return matches[i].ToolId < matches[j].ToolId
	})
	return matches
}

// Opt-in only: ordinary tests never contact the shared gateway. Capture fetches
// current route/tool metadata, never invokes a workload tool. Replay reuses that
// same temporary snapshot to compare ranking without live-catalog drift.
func TestMCPDiscoveryLiveObservation(t *testing.T) {
	dir := os.Getenv("AMG_SEARCH_OBSERVE_DIR")
	if dir == "" {
		t.Skip("set AMG_SEARCH_OBSERVE_DIR for authorized read-only observation")
	}
	mode := os.Getenv("AMG_SEARCH_OBSERVE_MODE")
	type routeObservation struct {
		Source       string `json:"source"`
		Tools        int    `json:"tools"`
		Milliseconds int64  `json:"milliseconds"`
		Failed       bool   `json:"failed"`
	}
	type sample struct {
		Kind     string   `json:"kind"`
		Query    string   `json:"query"`
		Expected string   `json:"expected,omitempty"`
		Top3     []string `json:"top3"`
		Hit      bool     `json:"hit"`
		Total    int      `json:"total"`
	}
	var entries []catalogEntry
	var routes []routeObservation
	var sources []UpstreamSource
	var failed int
	var cold, warm time.Duration
	snapshot := filepath.Join(dir, "catalog-snapshot.json")
	if mode == "capture" {
		app := &App{config: &ConfigStore{cfg: Config{SharedMCPBin: os.Getenv("AMG_SHARED_MCP_BIN")}}}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		start := time.Now()
		var err error
		sources, err = app.listSources(ctx)
		if err != nil {
			t.Fatal("route discovery failed")
		}
		var mu sync.Mutex
		fetch := func(ctx context.Context, src UpstreamSource) ([]json.RawMessage, error) {
			at := time.Now()
			tools, err := app.listRouteTools(ctx, src)
			mu.Lock()
			routes = append(routes, routeObservation{src.ID, len(tools), time.Since(at).Milliseconds(), err != nil})
			mu.Unlock()
			return tools, err
		}
		entries, failed, err = app.getLiveCatalogWith(ctx, sources, false, fetch)
		cold = time.Since(start)
		if err != nil {
			t.Fatal("catalog scan failed")
		}
		start = time.Now()
		_, _, err = app.getLiveCatalogWith(ctx, sources, false, fetch)
		warm = time.Since(start)
		if err != nil {
			t.Fatal("warm catalog failed")
		}
		bytes, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(snapshot, bytes, 0600); err != nil {
			t.Fatal(err)
		}
	} else if mode == "replay" || mode == "supplement" {
		bytes, err := os.ReadFile(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bytes, &entries); err != nil {
			t.Fatal(err)
		}
		if mode == "supplement" {
			app := &App{config: &ConfigStore{cfg: Config{SharedMCPBin: os.Getenv("AMG_SHARED_MCP_BIN")}}}
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			sources, err = app.listSources(ctx)
			if err != nil {
				t.Fatal("route discovery failed")
			}
			present := map[string]bool{}
			for _, entry := range entries {
				present[entry.SourceID] = true
			}
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, src := range sources {
				if present[src.ID] {
					continue
				}
				wg.Add(1)
				go func(src UpstreamSource) {
					defer wg.Done()
					start := time.Now()
					tools, err := app.listRouteTools(ctx, src)
					mu.Lock()
					defer mu.Unlock()
					routes = append(routes, routeObservation{src.ID, len(tools), time.Since(start).Milliseconds(), err != nil})
					if err != nil {
						failed++
						return
					}
					for _, raw := range tools {
						name, desc := rawToolInfo(raw)
						if name != "" {
							entries = append(entries, catalogEntry{SourceID: src.ID, Name: name, Description: desc, RawTool: raw})
						}
					}
				}(src)
			}
			wg.Wait()
			bytes, err = json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(snapshot, bytes, 0600); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		t.Fatal("mode must be capture, supplement or replay")
	}
	var samples []sample
	start := time.Now()
	for _, q := range mcpDiscoveryQueries {
		if q.kind != "new10" && q.kind != "holdout" && q.kind != "english" {
			continue
		}
		matches := sortedDiscoveryMatches(entries, q.query)
		s := sample{Kind: q.kind, Query: q.query, Expected: q.source, Total: len(matches)}
		for i, m := range matches {
			if i >= 3 {
				break
			}
			s.Top3 = append(s.Top3, m.SourceId+"/"+m.ToolId)
			if q.source != "" && m.SourceId == q.source {
				s.Hit = true
			}
		}
		samples = append(samples, s)
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Source < routes[j].Source })
	out := struct {
		Mode      string             `json:"mode"`
		Entries   int                `json:"entries"`
		Sources   int                `json:"sources"`
		Failed    int                `json:"failed"`
		ColdMS    int64              `json:"cold_ms"`
		WarmUS    int64              `json:"warm_us"`
		RankingMS int64              `json:"ranking_ms"`
		Routes    []routeObservation `json:"routes,omitempty"`
		Samples   []sample           `json:"samples"`
	}{mode, len(entries), len(sources), failed, cold.Milliseconds(), warm.Microseconds(), time.Since(start).Milliseconds(), routes, samples}
	bytes, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	label := os.Getenv("AMG_SEARCH_OBSERVE_LABEL")
	if label == "" {
		label = mode
	}
	if filepath.Base(label) != label {
		t.Fatal("invalid observation label")
	}
	if err := os.WriteFile(filepath.Join(dir, label+".json"), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("metadata only: mode=%s entries=%d sources=%d failed=%d cold=%s warm=%s", mode, len(entries), len(sources), failed, cold, warm)
}

func discoveryFixtureApp(t *testing.T, url string) *App {
	t.Helper()
	t.Setenv("AMG_DISCOVERY_FIXTURE_URL", url)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "route-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\"'\"'")+"' -test.run='^TestMCPDiscoveryHelperProcess$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return &App{config: &ConfigStore{cfg: Config{SharedMCPBin: wrapper}}}
}

func TestMCPDiscoveryHelperProcess(t *testing.T) {
	url := os.Getenv("AMG_DISCOVERY_FIXTURE_URL")
	if url == "" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "discovery-helper", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_routes"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"routes": []map[string]any{{"name": "fixture", "path": "/mcp/fixture", "url": url}}}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "mcp_list_tools"}, func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		if in["tool_name"] == "exact_tool" || in["tool_name"] == "wrong_requested" {
			return nil, map[string]any{"tool": map[string]any{"name": "exact_tool", "inputSchema": map[string]any{"type": "object", "description": "EXACT_FALLBACK_MARKER"}}}, nil
		}
		return nil, map[string]any{"tools": []map[string]any{{"name": "exact_tool", "description": "legacy summary"}}, "compact": true}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestMCPDiscoveryCompactExactFallback(t *testing.T) {
	app := discoveryFixtureApp(t, "http://fixture.invalid/mcp")
	sources := []UpstreamSource{{ID: "fixture", Name: "fixture", URL: "http://fixture.invalid/mcp"}}
	fetch := func(context.Context, UpstreamSource) ([]json.RawMessage, error) {
		return []json.RawMessage{json.RawMessage(`{"name":"exact_tool","description":"legacy summary"}`)}, nil
	}
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "compact-contract", Version: "1"}, nil)
	app.registerMCPParity(server)
	result, err := contractClient(t, server).CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{"source": "fixture", "tool": "exact_tool"}})
	if err != nil || result.IsError || !strings.Contains(toolText(result), "EXACT_FALLBACK_MARKER") {
		t.Fatalf("compact summary masqueraded as exact schema: err=%v result=%+v", err, result)
	}
	wrong, err := contractClient(t, server).CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{"source": "fixture", "tool": "wrong_requested"}})
	if err != nil || !wrong.IsError {
		t.Fatalf("different tool schema accepted as exact: err=%v result=%+v", err, wrong)
	}
}

func TestMCPDiscoveryFailureDetailsAndStale(t *testing.T) {
	app := discoveryFixtureApp(t, "http://fixture.invalid/mcp")
	sources := []UpstreamSource{{ID: "fixture", Name: "fixture", URL: "http://fixture.invalid/mcp"}}
	fetch := func(context.Context, UpstreamSource) ([]json.RawMessage, error) {
		return []json.RawMessage{json.RawMessage(`{"name":"read_note","inputSchema":{"type":"object"}}`)}, nil
	}
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch); err != nil {
		t.Fatal(err)
	}
	outage := func(context.Context, UpstreamSource) ([]json.RawMessage, error) {
		return nil, errors.New("private http://user:secret@fixture.invalid token=SECRET")
	}
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, true, outage); err != nil {
		t.Fatal(err)
	}
	out, err := app.searchRouteTools(context.Background(), SearchMCPInput{Query: "note"})
	if err != nil {
		t.Fatal(err)
	}
	obj := contractJSON(t, out)
	failures, _ := obj["sourceFailures"].([]any)
	if !out.Incomplete || obj["stale"] != true || len(failures) != 1 {
		t.Fatalf("outage lost failure/stale evidence: %s", mustContractJSON(t, out))
	}
	failure := failures[0].(map[string]any)
	if failure["source"] != "fixture" || failure["stage"] == "" || failure["kind"] == "" || failure["durationMs"] == nil {
		t.Fatalf("failure diagnosis is incomplete: %+v", failure)
	}
	if strings.Contains(mustContractJSON(t, out), "SECRET") || strings.Contains(mustContractJSON(t, out), "fixture.invalid") {
		t.Fatal("raw upstream diagnostics leaked")
	}
	if _, ok := app.cachedRoute(sources, "fixture", false); ok {
		t.Fatal("stale schema reused as exact")
	}
}

func TestMCPDiscoverySDKErrorStagesAndCancellation(t *testing.T) {
	t.Run("initialize_503", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "private token=SECRET", http.StatusServiceUnavailable)
		}))
		defer upstream.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := (&App{}).listRouteTools(ctx, UpstreamSource{URL: upstream.URL})
		var stage *catalogStageError
		if !errors.As(err, &stage) || stage.stage != "route_initialize" || catalogErrorKind(err) != "http_503" || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), upstream.URL) {
			t.Fatalf("HTTP failure lost safe stage/kind: %v", err)
		}
	})
	t.Run("tools_list", func(t *testing.T) {
		server := mcp.NewServer(&mcp.Implementation{Name: "failed-list", Version: "1"}, nil)
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					return nil, errors.New("private token=SECRET")
				}
				return next(ctx, method, req)
			}
		})
		upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
		defer upstream.Close()
		_, err := (&App{}).listRouteTools(context.Background(), UpstreamSource{URL: upstream.URL})
		var stage *catalogStageError
		if !errors.As(err, &stage) || stage.stage != "tools_list" || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("list error lost safe stage: %v", err)
		}
	})
	t.Run("tools_list_cancel", func(t *testing.T) {
		server := mcp.NewServer(&mcp.Implementation{Name: "slow-list", Version: "1"}, nil)
		release := make(chan struct{})
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" {
					select {
					case <-ctx.Done():
					case <-release:
					}
					return nil, errors.New("SDK transport erased context type")
				}
				return next(ctx, method, req)
			}
		})
		upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
		defer upstream.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		_, err := (&App{}).listRouteTools(ctx, UpstreamSource{URL: upstream.URL})
		var stage *catalogStageError
		if !errors.As(err, &stage) || stage.stage != "tools_list" || catalogErrorKind(err) != "timeout" {
			t.Fatalf("list deadline lost: %v", err)
		}
	})
	t.Run("initialize_cancel", func(t *testing.T) {
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer upstream.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := (&App{}).listRouteTools(ctx, UpstreamSource{URL: upstream.URL})
		if catalogErrorKind(err) != "timeout" || time.Since(start) > 6*time.Second {
			t.Fatalf("initialize cancellation lost: %v", err)
		}
	})
	t.Run("context_isolation_and_cancel", func(t *testing.T) {
		type outerKey struct{}
		parent, cancel := context.WithCancel(context.WithValue(context.Background(), outerKey{}, "outer-protocol"))
		ctx, stop := upstreamContext(parent, time.Second)
		defer stop()
		if ctx.Value(outerKey{}) != nil {
			t.Fatal("outer MCP values leaked upstream")
		}
		cancel()
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("parent cancellation lost")
		}
	})
}

func TestMCPDiscoverySDKMetadataLimits(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "oversized", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "oversized", InputSchema: map[string]any{"type": "object", "description": strings.Repeat("x", catalogMaxMetadataBytes+1)}}, nil)
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	defer upstream.Close()
	_, err := (&App{}).listRouteTools(context.Background(), UpstreamSource{URL: upstream.URL})
	if err == nil || catalogErrorKind(err) != "metadata_limit" {
		t.Fatalf("oversized JSON metadata not bounded: %v", err)
	}
}

func TestMCPDiscoverySDKFullSchemasOnce(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "1"}, &mcp.ServerOptions{PageSize: 1})
	for _, name := range []string{"exact_tool", "second_tool"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object", "description": "FULL_SCHEMA_" + strings.Repeat("metadata", 20000)}}, nil)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	upstream := httptest.NewServer(handler)
	defer upstream.Close()
	app := discoveryFixtureApp(t, upstream.URL)
	sources := []UpstreamSource{{ID: "fixture", Name: "fixture", URL: upstream.URL}}
	rows, failed, err := app.getLiveCatalog(context.Background(), sources, false)
	if err != nil || failed != 0 || len(rows) != 2 {
		t.Fatalf("full paginated catalog lost: rows=%d failed=%d err=%v", len(rows), failed, err)
	}
	for _, row := range rows {
		if !strings.Contains(string(row.RawTool), "FULL_SCHEMA_") {
			t.Fatal("SDK catalog lost exact schema")
		}
	}
	outer := mcp.NewServer(&mcp.Implementation{Name: "outer", Version: "1"}, nil)
	app.registerMCPParity(outer)
	result, err := contractClient(t, outer).CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{"source": "fixture", "tool": "exact_tool"}})
	if err != nil || result.IsError || !strings.Contains(toolText(result), "FULL_SCHEMA_") || strings.Contains(toolText(result), "EXACT_FALLBACK_MARKER") {
		t.Fatalf("get_tool repeated catalog initialization instead of exact cache: err=%v", err)
	}
}

func TestMCPDiscoveryCachedSourceAndExactSchema(t *testing.T) {
	const fixtureURL = "http://catalog.fixture.invalid/mcp"
	t.Setenv("AMG_CONTRACT_HELPER", "1")
	t.Setenv("AMG_CONTRACT_UPSTREAM", fixtureURL)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "route-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\"'\"'")+"' -test.run='^TestToolContractsRouteHelperProcess$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	app := &App{config: &ConfigStore{cfg: Config{SharedMCPBin: wrapper}}}
	sources := []UpstreamSource{{ID: "fixture", Name: "fixture", URL: fixtureURL}}
	raw := json.RawMessage(`{"name":"exact_cached_tool","description":"Read cached note","inputSchema":{"type":"object","properties":{"query":{"type":"string","description":"exact parameter"}}}}`)
	fetch := func(context.Context, UpstreamSource) ([]json.RawMessage, error) { return []json.RawMessage{raw}, nil }
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch); err != nil {
		t.Fatal(err)
	}
	got, err := app.searchRouteTools(context.Background(), SearchMCPInput{Query: "note", Source: "fixture"})
	if err != nil || len(got.Matches) != 1 || got.Matches[0].Name != "exact_cached_tool" {
		t.Fatalf("valid source cache missed: %+v %v", got, err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "cache-contract", Version: "1"}, nil)
	app.registerMCPParity(server)
	cs := contractClient(t, server)
	result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_get_tool", Arguments: map[string]any{"source": "fixture", "tool": "exact_cached_tool"}})
	if err != nil || result.IsError || !strings.Contains(toolText(result), "exact parameter") {
		t.Fatalf("exact cached schema missed: %v %+v", err, result)
	}
	// The fixture helper only exposes routes. A directory request therefore fails,
	// proving force and changed signatures bypass a formerly successful cache.
	if _, err := app.searchRouteTools(context.Background(), SearchMCPInput{Query: "note", Source: "fixture", ForceRefresh: true}); err == nil {
		t.Fatal("force_refresh silently reused source cache")
	}
	t.Setenv("AMG_CONTRACT_UPSTREAM", fixtureURL+"/changed")
	if _, err := app.searchRouteTools(context.Background(), SearchMCPInput{Query: "note", Source: "fixture"}); err == nil {
		t.Fatal("route URL change silently reused source cache")
	}
}
