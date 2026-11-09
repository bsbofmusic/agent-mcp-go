package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SourceQuery struct {
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type SearchMCPInput struct {
	Query        string `json:"query"`
	Source       string `json:"source,omitempty"`
	Limit        int    `json:"limit,omitempty"`
	ForceRefresh bool   `json:"force_refresh,omitempty"`
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
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	ToolCount int    `json:"toolCount"`
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
type SearchMCPOut struct {
	Matches []SearchMatch `json:"matches"`
	Query   string        `json:"query"`
	Source  string        `json:"source,omitempty"`
	Total   int           `json:"total"`
	Hint    string        `json:"hint,omitempty"`
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
	return nil, fmt.Errorf("unexpected shared MCP result envelope")
}
func (a *App) listSources(ctx context.Context) ([]UpstreamSource, error) {
	v, e := a.sharedCall(ctx, "mcp_list_routes", map[string]any{})
	if e != nil {
		return nil, e
	}
	b, e := unpackSharedOutput(v)
	if e != nil {
		return nil, e
	}
	var data struct {
		Routes []struct {
			Name string `json:"name"`
			Path string `json:"path"`
			URL  string `json:"url"`
		} `json:"routes"`
	}
	if e = json.Unmarshal(b, &data); e != nil {
		return nil, e
	}
	var sources []UpstreamSource
	for _, v := range data.Routes {
		if v.URL == "" || v.Name == "" {
			continue
		}
		sources = append(sources, UpstreamSource{ID: strings.TrimPrefix(v.Path, "/mcp/"), Name: v.Name, URL: v.URL})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	return sources, nil
}
func selectSource(sources []UpstreamSource, id string) (UpstreamSource, error) {
	for _, s := range sources {
		if s.ID == id || s.Name == id {
			return s, nil
		}
	}
	return UpstreamSource{}, errors.New("unknown MCP route source; call mcp_list_sources")
}
func (a *App) listRouteTools(ctx context.Context, source UpstreamSource) ([]json.RawMessage, error) {
	v, e := a.sharedCall(ctx, "mcp_list_tools", map[string]any{"server": source.URL, "timeout_seconds": 30})
	if e != nil {
		return nil, e
	}
	b, e := unpackSharedOutput(v)
	if e != nil {
		return nil, e
	}
	var data struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if e = json.Unmarshal(b, &data); e != nil {
		return nil, e
	}
	return data.Tools, nil
}
func rawToolInfo(raw json.RawMessage) (string, string) {
	var v struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Name, v.Description
}

func decodedTool(raw json.RawMessage) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("upstream schema is not a JSON object")
	}
	return out, nil
}
func (a *App) searchRouteTools(ctx context.Context, in SearchMCPInput) (SearchMCPOut, error) {
	if len(searchQueryTokens(in.Query)) == 0 {
		return SearchMCPOut{}, errors.New("query is required")
	}
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit < 1 || in.Limit > 50 {
		return SearchMCPOut{}, errors.New("limit must be 1..50")
	}
	sources, e := a.listSources(ctx)
	if e != nil {
		return SearchMCPOut{}, e
	}
	chosen := []UpstreamSource{}
	if in.Source != "" {
		src, e := selectSource(sources, in.Source)
		if e != nil {
			return SearchMCPOut{}, e
		}
		chosen = append(chosen, src)
	} else {
		chosen = sources
	}
	var entries []catalogEntry
	failed := 0
	if in.Source == "" {
		entries, failed, e = a.getLiveCatalog(ctx, chosen, in.ForceRefresh)
		if e != nil {
			return SearchMCPOut{}, e
		}
	} else {
		step, cancel := context.WithTimeout(ctx, 30*time.Second)
		tools, err := a.listRouteTools(step, chosen[0])
		cancel()
		if err != nil {
			return SearchMCPOut{}, err
		}
		for _, tool := range tools {
			name, desc := rawToolInfo(tool)
			if name != "" {
				entries = append(entries, catalogEntry{SourceID: chosen[0].ID, Name: name, Description: desc, SearchText: desc + " " + string(tool), RawTool: tool})
			}
		}
	}
	matches := rankMCPEntries(entries, in.Query)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score == matches[j].Score {
			if matches[i].SourceId == matches[j].SourceId {
				return matches[i].ToolId < matches[j].ToolId
			}
			return matches[i].SourceId < matches[j].SourceId
		}
		return matches[i].Score > matches[j].Score
	})
	count := len(matches)
	if len(matches) > in.Limit {
		matches = matches[:in.Limit]
	}
	hint := fmt.Sprintf("Checked %d canonical routes, %d temporarily unavailable. Catalog is an ephemeral in-process cache (2 min, 15s on partial failure); force_refresh bypasses cache. Select source for exact schema.", len(chosen), failed)
	return SearchMCPOut{Matches: matches, Query: in.Query, Source: in.Source, Total: count, Hint: hint}, nil
}

func (a *App) registerMCPParity(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_list_sources", Description: "Discover current AgentGateway-backed MCP sources on demand, without a duplicate registry."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SourceQuery) (*mcp.CallToolResult, SourcesOut, error) {
			sources, e := a.listSources(ctx)
			if e != nil {
				return nil, SourcesOut{}, e
			}
			var result []UpstreamSource
			for _, v := range sources {
				if in.Query == "" || strings.Contains(strings.ToLower(v.ID+" "+v.Name), strings.ToLower(in.Query)) {
					result = append(result, v)
				}
			}
			count := len(result)
			if in.Limit > 0 && len(result) > in.Limit {
				result = result[:in.Limit]
			}
			return nil, SourcesOut{Sources: result, Total: count}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_search_tools", Description: "BM25-like ranked search of all live AgentGateway MCP route tool names, descriptions and schemas. The ephemeral cache invalidates on route changes; force_refresh bypasses its 2-minute TTL."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SearchMCPInput) (*mcp.CallToolResult, SearchMCPOut, error) {
			v, e := a.searchRouteTools(ctx, in)
			return nil, v, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_get_tool", Description: "Return the exact upstream MCP tool schema for one source/tool pair."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GetMCPInput) (*mcp.CallToolResult, GetMCPOut, error) {
			sources, e := a.listSources(ctx)
			if e != nil {
				return nil, GetMCPOut{}, e
			}
			src, e := selectSource(sources, in.Source)
			if e != nil {
				return nil, GetMCPOut{}, e
			}
			v, e := a.sharedCall(ctx, "mcp_list_tools", map[string]any{"server": src.URL, "tool_name": in.Tool})
			if e != nil {
				return nil, GetMCPOut{}, e
			}
			raw, e := unpackSharedOutput(v)
			if e != nil {
				return nil, GetMCPOut{}, e
			}
			var data struct {
				Tools []json.RawMessage `json:"tools"`
				Tool  json.RawMessage   `json:"tool"`
			}
			if e = json.Unmarshal(raw, &data); e != nil {
				return nil, GetMCPOut{}, e
			}
			for _, item := range data.Tools {
				name, _ := rawToolInfo(item)
				if name == in.Tool {
					obj, err := decodedTool(item)
					return nil, GetMCPOut{Source: src, Tool: obj}, err
				}
			}
			if len(data.Tool) > 0 {
				obj, err := decodedTool(data.Tool)
				return nil, GetMCPOut{Source: src, Tool: obj}, err
			}
			// Older helper returns a standalone schema rather than {tool}.
			name, _ := rawToolInfo(raw)
			if name == in.Tool {
				obj, err := decodedTool(raw)
				return nil, GetMCPOut{Source: src, Tool: obj}, err
			}
			return nil, GetMCPOut{}, errors.New("tool schema not found in selected source")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_call_tool", Description: "Invoke a real tool in one dynamic AgentGateway MCP route and preserve its native MCP content blocks and metadata."},
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
			// The upstream connection negotiates its own MCP version. Passing
			// inbound values makes the SDK reuse the outer protocol header
			// during a legacy initialize fallback.
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			callCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			stopCancel := context.AfterFunc(ctx, cancel)
			defer stopCancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "agent-mcp-go-route-client", Version: version}, nil)
			conn, e := client.Connect(callCtx, &mcp.StreamableClientTransport{
				Endpoint:             src.URL,
				DisableStandaloneSSE: true,
			}, nil)
			if e != nil {
				return nil, nil, e
			}
			defer conn.Close()
			v, e := conn.CallTool(callCtx, &mcp.CallToolParams{Name: in.Tool, Arguments: args})
			return v, nil, e
		})
}
