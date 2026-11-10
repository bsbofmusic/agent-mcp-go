package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SourceQuery struct {
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type SearchMCPInput struct {
	Query        string `json:"query,omitempty" jsonschema:"Optional literal name/description filter when source is selected; required for unscoped fallback."`
	Source       string `json:"source,omitempty" jsonschema:"Module ID from mcp_list_sources; select this before listing tools."`
	Limit        int    `json:"limit,omitempty"`
	ForceRefresh bool   `json:"force_refresh,omitempty" jsonschema:"Compatibility input; directory requests are always fresh."`
}
type GetMCPInput struct {
	Source string `json:"source"`
	Tool   string `json:"tool"`
}
type CallMCPInput struct {
	Source    string         `json:"source"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}
type UpstreamSource struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	ToolCount   *int   `json:"toolCount,omitempty"`
}
type SourcesOut struct {
	Sources []UpstreamSource `json:"sources"`
	Total   int              `json:"total"`
}
type SearchMatch struct {
	SourceId    string  `json:"sourceId"`
	ToolId      string  `json:"toolId"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Score       float64 `json:"score"`
}
type CatalogSourceFailure struct {
	Source     string `json:"source"`
	Stage      string `json:"stage"`
	Kind       string `json:"kind"`
	DurationMS int64  `json:"durationMs"`
}
type SearchMCPOut struct {
	Matches            []SearchMatch          `json:"matches"`
	Query              string                 `json:"query"`
	Source             string                 `json:"source,omitempty"`
	Total              int                    `json:"total"`
	Hint               string                 `json:"hint,omitempty"`
	Incomplete         bool                   `json:"incomplete,omitempty"`
	UnavailableSources []string               `json:"unavailableSources,omitempty"`
	SourceFailures     []CatalogSourceFailure `json:"sourceFailures,omitempty"`
	Stale              bool                   `json:"stale,omitempty"`
}
type GetMCPOut struct {
	Source UpstreamSource `json:"source"`
	Tool   map[string]any `json:"tool"`
}

func unpackSharedOutput(out TextOut) ([]byte, error) {
	var outer struct {
		Output string `json:"output"`
	}
	if json.Unmarshal([]byte(out.Output), &outer) == nil && outer.Output != "" {
		return []byte(outer.Output), nil
	}
	if json.Valid([]byte(out.Output)) {
		return []byte(out.Output), nil
	}
	return nil, errors.New("unexpected shared MCP result envelope")
}
func (a *App) listSources(ctx context.Context) ([]UpstreamSource, error) {
	logRequestPhase(ctx, "route_discovery")
	v, err := a.sharedCall(ctx, "mcp_list_routes", map[string]any{})
	if err != nil {
		return nil, err
	}
	raw, err := unpackSharedOutput(v)
	if err != nil {
		return nil, err
	}
	var data struct {
		Routes []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			URL         string `json:"url"`
			Description string `json:"description"`
			ToolCount   *int   `json:"toolCount"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	sources := []UpstreamSource{}
	for _, route := range data.Routes {
		if route.URL != "" && route.Name != "" {
			sources = append(sources, UpstreamSource{ID: strings.TrimPrefix(route.Path, "/mcp/"), Name: route.Name, URL: route.URL, Description: route.Description, ToolCount: route.ToolCount})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	return sources, nil
}
func selectSource(sources []UpstreamSource, id string) (UpstreamSource, error) {
	for _, source := range sources {
		if source.ID == id || source.Name == id {
			return source, nil
		}
	}
	return UpstreamSource{}, errors.New("unknown MCP route source; call mcp_list_sources")
}

const (
	moduleRouteTimeout = 30 * time.Second
	moduleScanTimeout  = 45 * time.Second
	moduleScanWorkers  = 8
)

func moduleErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "upstream_error"
	}
}

type moduleDirectoryError struct{ err error }

func (e *moduleDirectoryError) Error() string {
	return "module directory failed (" + moduleErrorKind(e.err) + ")"
}
func (e *moduleDirectoryError) Unwrap() error { return e.err }

func (a *App) listRouteTools(parent context.Context, source UpstreamSource) ([]json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(parent, moduleRouteTimeout)
	defer cancel()
	out, err := a.sharedCall(ctx, "mcp_list_tools", map[string]any{"server": source.URL, "timeout_seconds": 30})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, &moduleDirectoryError{err}
	}
	raw, err := unpackSharedOutput(out)
	if err != nil {
		return nil, &moduleDirectoryError{err}
	}
	var data struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, &moduleDirectoryError{err}
	}
	return data.Tools, nil
}

// The fallback inventory belongs only to this request; selected modules skip it.
func scanModuleTools(parent context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) (SearchMCPOut, error) {
	if err := parent.Err(); err != nil {
		return SearchMCPOut{}, err
	}
	ctx, cancel := context.WithTimeout(parent, moduleScanTimeout)
	defer cancel()
	type result struct {
		source  string
		tools   []json.RawMessage
		err     error
		elapsed time.Duration
	}
	jobs := make(chan UpstreamSource)
	results := make(chan result, len(sources))
	workers := min(moduleScanWorkers, len(sources))
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for source := range jobs {
				step, stop := context.WithTimeout(ctx, moduleRouteTimeout)
				start := time.Now()
				tools, err := list(step, source)
				if step.Err() != nil {
					err = step.Err()
				}
				stop()
				results <- result{source.ID, tools, err, time.Since(start)}
			}
		}()
	}
	go func() {
		defer close(results)
		defer wg.Wait()
		defer close(jobs)
		for _, source := range sources {
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- source:
			case <-ctx.Done():
				return
			}
		}
	}()
	out := SearchMCPOut{Matches: []SearchMatch{}}
	seen := map[string]bool{}
	started := time.Now()
	for found := range results {
		seen[found.source] = true
		if found.err != nil {
			out.UnavailableSources = append(out.UnavailableSources, found.source)
			out.SourceFailures = append(out.SourceFailures, CatalogSourceFailure{found.source, "tools_list", moduleErrorKind(found.err), found.elapsed.Milliseconds()})
			continue
		}
		out.Matches = append(out.Matches, moduleSummaries(found.source, found.tools)...)
	}
	for _, source := range sources {
		if !seen[source.ID] {
			out.UnavailableSources = append(out.UnavailableSources, source.ID)
			out.SourceFailures = append(out.SourceFailures, CatalogSourceFailure{source.ID, "scan_queue", moduleErrorKind(ctx.Err()), time.Since(started).Milliseconds()})
		}
	}
	sort.Strings(out.UnavailableSources)
	sort.Slice(out.SourceFailures, func(i, j int) bool { return out.SourceFailures[i].Source < out.SourceFailures[j].Source })
	out.Incomplete = len(out.UnavailableSources) > 0
	return out, nil
}

func moduleSummaries(source string, tools []json.RawMessage) []SearchMatch {
	matches := []SearchMatch{}
	for _, raw := range tools {
		var tool struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.Unmarshal(raw, &tool) == nil && tool.Name != "" {
			matches = append(matches, SearchMatch{SourceId: source, ToolId: tool.Name, Name: tool.Name, Description: tool.Description, Score: 1})
		}
	}
	return matches
}

func (a *App) searchRouteTools(ctx context.Context, in SearchMCPInput) (SearchMCPOut, error) {
	query := strings.ToLower(strings.TrimSpace(in.Query))
	if in.Source == "" && query == "" {
		return SearchMCPOut{}, errors.New("query is required without source; select a module with mcp_list_sources")
	}
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit < 1 || in.Limit > 50 {
		return SearchMCPOut{}, errors.New("limit must be 1..50")
	}
	sources, err := a.listSources(ctx)
	if err != nil {
		return SearchMCPOut{}, err
	}
	out := SearchMCPOut{Matches: []SearchMatch{}}
	if in.Source != "" {
		source, err := selectSource(sources, in.Source)
		if err != nil {
			return SearchMCPOut{}, err
		}
		tools, err := a.listRouteTools(ctx, source)
		if err != nil {
			return SearchMCPOut{}, err
		}
		out.Matches = moduleSummaries(source.ID, tools)
	} else {
		logRequestPhase(ctx, "module_scan")
		out, err = scanModuleTools(withoutRequestTrace(ctx), sources, a.listRouteTools)
		if err != nil {
			return SearchMCPOut{}, err
		}
	}
	matches := []SearchMatch{}
	for _, match := range out.Matches {
		if query == "" || strings.Contains(strings.ToLower(match.Name+" "+match.Description), query) {
			matches = append(matches, match)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].SourceId == matches[j].SourceId {
			return matches[i].ToolId < matches[j].ToolId
		}
		return matches[i].SourceId < matches[j].SourceId
	})
	out.Total = len(matches)
	if len(matches) > in.Limit {
		matches = matches[:in.Limit]
	}
	out.Matches, out.Query, out.Source = matches, in.Query, in.Source
	out.Hint = "Fresh canonical helper summaries, ordered by source/name; score=1 is compatibility only. Query is a literal name/description filter. Select a module before listing tools; use mcp_get_tool for an exact schema. force_refresh is accepted for compatibility; no directory cache is retained."
	return out, nil
}

func hasExactToolSchema(raw json.RawMessage) bool {
	var tool struct {
		InputSchema map[string]json.RawMessage `json:"inputSchema"`
	}
	return json.Unmarshal(raw, &tool) == nil && tool.InputSchema != nil
}
func decodedTool(raw json.RawMessage) (map[string]any, error) {
	var tool map[string]any
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, err
	}
	if tool == nil {
		return nil, errors.New("upstream schema is not a JSON object")
	}
	return tool, nil
}
func exactModuleTool(raw json.RawMessage, name string) (map[string]any, error) {
	var data struct {
		Tools []json.RawMessage `json:"tools"`
		Tool  json.RawMessage   `json:"tool"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	for _, candidate := range append(append(data.Tools, data.Tool), raw) {
		var identity struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(candidate, &identity) == nil && identity.Name == name && hasExactToolSchema(candidate) {
			return decodedTool(candidate)
		}
	}
	return nil, errors.New("tool schema not found in selected source")
}

// An upstream call negotiates its own version, without inbound MCP values.
func upstreamContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if earlier, ok := parent.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	stop := context.AfterFunc(parent, cancel)
	return ctx, func() { stop(); cancel() }
}

func (a *App) registerMCPParity(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_list_sources", Description: "Discover current canonical MCP modules without initializing their targets. Only upstream-published metadata is returned; absent toolCount means unknown."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SourceQuery) (*mcp.CallToolResult, SourcesOut, error) {
			sources, err := a.listSources(ctx)
			if err != nil {
				return nil, SourcesOut{}, err
			}
			result := []UpstreamSource{}
			for _, source := range sources {
				if in.Query == "" || strings.Contains(strings.ToLower(source.ID+" "+source.Name), strings.ToLower(in.Query)) {
					result = append(result, source)
				}
			}
			count := len(result)
			if in.Limit > 0 && len(result) > in.Limit {
				result = result[:in.Limit]
			}
			return nil, SourcesOut{Sources: result, Total: count}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_search_tools", Description: "List fresh tool summaries in one selected MCP module; source plus optional literal query is the normal path. Without source, a query triggers a bounded literal fallback across current modules, not semantic routing. Results are ordered by source/name; no directory cache."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SearchMCPInput) (*mcp.CallToolResult, SearchMCPOut, error) {
			v, err := a.searchRouteTools(ctx, in)
			return nil, v, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_get_tool", Description: "Fetch the current exact same-name upstream schema for one selected source/tool pair, including annotations and metadata."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GetMCPInput) (*mcp.CallToolResult, GetMCPOut, error) {
			if in.Tool == "" {
				return nil, GetMCPOut{}, errors.New("tool is required")
			}
			sources, err := a.listSources(ctx)
			if err != nil {
				return nil, GetMCPOut{}, err
			}
			source, err := selectSource(sources, in.Source)
			if err != nil {
				return nil, GetMCPOut{}, err
			}
			step, cancel := context.WithTimeout(ctx, moduleRouteTimeout)
			defer cancel()
			out, err := a.sharedCall(step, "mcp_list_tools", map[string]any{"server": source.URL, "tool_name": in.Tool, "timeout_seconds": 30})
			if err != nil {
				return nil, GetMCPOut{}, err
			}
			raw, err := unpackSharedOutput(out)
			if err != nil {
				return nil, GetMCPOut{}, err
			}
			tool, err := exactModuleTool(raw, in.Tool)
			return nil, GetMCPOut{Source: source, Tool: tool}, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_call_tool", Description: "Invoke a known tool in one current canonical MCP module without a tools/list preflight. Preserve native MCP content blocks and metadata."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in CallMCPInput) (*mcp.CallToolResult, any, error) {
			sources, e := a.listSources(ctx)
			if e != nil {
				return nil, nil, e
			}
			src, e := selectSource(sources, in.Source)
			if e != nil {
				return nil, nil, e
			}
			if in.Tool == "" {
				return nil, nil, errors.New("tool is required")
			}
			args := in.Arguments
			if args == nil {
				args = map[string]any{}
			}
			// Inbound values would reuse the outer protocol header during a legacy initialize fallback.
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			callCtx, cancel := upstreamContext(ctx, 120*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "agent-mcp-go-route-client", Version: version}, nil)
			logRequestPhase(ctx, "upstream_connect")
			conn, e := client.Connect(callCtx, &mcp.StreamableClientTransport{Endpoint: src.URL, DisableStandaloneSSE: true}, nil)
			if e != nil {
				return nil, nil, e
			}
			defer func() {
				logRequestPhase(ctx, "upstream_close")
				_ = conn.Close()
			}()
			logRequestPhase(ctx, "upstream_call")
			v, e := conn.CallTool(callCtx, &mcp.CallToolParams{Name: in.Tool, Arguments: args})
			return v, nil, e
		})
}
