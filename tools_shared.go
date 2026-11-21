package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type SharedListInput struct {
	Server   string `json:"server" jsonschema:"URL of the local AgentGateway MCP route this call targets."`
	ToolName string `json:"tool_name,omitempty"`
}
type SharedCallInput struct {
	Server        string `json:"server"`
	Tool          string `json:"tool"`
	ArgumentsJSON string `json:"arguments_json,omitempty" jsonschema:"Target tool argument JSON object string; default {}."`
}

func (a *App) sharedCall(ctx context.Context, tool string, args map[string]any) (TextOut, error) {
	exe := a.config.Get().SharedMCPBin
	if exe == "" {
		return TextOut{}, errors.New("shared_mcp_bin not configured")
	}
	if _, err := os.Stat(exe); err != nil {
		return TextOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.config.Get().CommandTimeoutSec)*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-mcp-go-shared-client", Version: version}, nil)
	command := exec.Command(exe)
	command.Env = executionEnvironment(a.config.Get())
	logRequestPhase(ctx, "helper_connect")
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		return TextOut{}, fmt.Errorf("shared MCP client startup: %w", err)
	}
	defer func() {
		logRequestPhase(ctx, "helper_close")
		_ = session.Close()
	}()
	logRequestPhase(ctx, "helper_call")
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return TextOut{}, err
	}
	if result.IsError {
		for _, v := range result.Content {
			if t, ok := v.(*mcp.TextContent); ok {
				return TextOut{}, fmt.Errorf("upstream tool: %s", t.Text)
			}
		}
		return TextOut{}, errors.New("shared upstream tool failed")
	}
	if result.StructuredContent != nil {
		b, e := json.Marshal(result.StructuredContent)
		if e == nil {
			return TextOut{string(b)}, nil
		}
	}
	for _, v := range result.Content {
		if t, ok := v.(*mcp.TextContent); ok {
			return TextOut{t.Text}, nil
		}
	}
	return TextOut{"{}"}, nil
}
func (a *App) registerShared(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "shared_mcp_routes", Title: "Shared routes", Description: "List the shared MCP helper routes registered on this host."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, TextOut, error) {
			out, err := a.sharedCall(ctx, "mcp_list_routes", map[string]any{})
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "shared_mcp_tools", Title: "Shared tools", Description: "List the tools exposed by one shared MCP helper route."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SharedListInput) (*mcp.CallToolResult, TextOut, error) {
			if in.Server == "" {
				return nil, TextOut{}, errors.New("server is required")
			}
			args := map[string]any{"server": in.Server}
			if in.ToolName != "" {
				args["tool_name"] = in.ToolName
			}
			out, err := a.sharedCall(ctx, "mcp_list_tools", args)
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "shared_mcp_call", Title: "Shared call", Description: "Call one tool on a shared MCP helper route and return its text result."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SharedCallInput) (*mcp.CallToolResult, TextOut, error) {
			if in.Server == "" || in.Tool == "" {
				return nil, TextOut{}, errors.New("server and tool required")
			}
			raw := in.ArgumentsJSON
			if raw == "" {
				raw = "{}"
			}
			var value map[string]any
			if err := json.Unmarshal([]byte(raw), &value); err != nil || value == nil {
				return nil, TextOut{}, errors.New("arguments_json must be a JSON object string")
			}
			out, err := a.sharedCall(ctx, "mcp_call_json", map[string]any{"server": in.Server, "tool": in.Tool, "arguments_json": raw})
			return nil, out, err
		})
}
