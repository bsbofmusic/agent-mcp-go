package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var toolChunkCounter atomic.Uint64

// Run after the SDK's typed output validation, retaining the existing Go output
// alias while advertising the canonical text contract to remote clients.
func toolContractsMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil {
			return result, err
		}
		switch method {
		case "tools/list":
			if listed, ok := result.(*mcp.ListToolsResult); ok && listed != nil {
				copy := *listed
				copy.Tools = make([]*mcp.Tool, len(listed.Tools))
				for i, tool := range listed.Tools {
					if tool == nil {
						continue
					}
					clone := *tool // SDK tools/list returns shared registration pointers.
					if nativeTextTool(tool.Name) {
						clone.OutputSchema = textContractSchema(tool.OutputSchema)
					} else if tool.Name == "exec_command" || tool.Name == "write_stdin" {
						if schema, ok := contractObject(tool.OutputSchema); ok {
							if props, ok := schema["properties"].(map[string]any); ok {
								props["chunk_id"] = map[string]any{"type": "string"}
								clone.OutputSchema = schema
							}
						}
					}
					if annotations := nativeToolAnnotations(tool.Name); annotations != nil {
						if tool.Annotations != nil {
							annotations.Title = tool.Annotations.Title
						}
						clone.Annotations = annotations
					}
					copy.Tools[i] = &clone
				}
				return &copy, nil
			}
		case "tools/call":
			call, ok := req.(*mcp.CallToolRequest)
			res, resultOK := result.(*mcp.CallToolResult)
			if !ok || call == nil || call.Params == nil || !resultOK || res == nil {
				break
			}
			name := call.Params.Name
			if !nativeTextTool(name) && name != "exec_command" && name != "write_stdin" {
				break
			}
			obj, ok := contractObject(res.StructuredContent)
			if !ok { // SDK validation/execution errors have no structured output.
				break
			}
			var text string
			if nativeTextTool(name) {
				text, ok = obj["content"].(string)
				if !ok {
					text, ok = obj["output"].(string)
				}
				if !ok {
					break
				}
				obj["content"], obj["output"] = text, text
			} else {
				if _, ok := obj["output"].(string); !ok {
					break
				}
				if _, ok := obj["chunk_id"].(string); !ok {
					obj["chunk_id"] = fmt.Sprintf("chunk-%d", toolChunkCounter.Add(1))
				}
				encoded, err := json.MarshalIndent(obj, "", "  ")
				if err != nil {
					return nil, err
				}
				text = string(encoded)
			}
			copy := *res
			copy.StructuredContent = obj
			if len(res.Content) == 1 {
				if original, ok := res.Content[0].(*mcp.TextContent); ok {
					block := *original
					block.Text = text
					copy.Content = []mcp.Content{&block}
				}
			} else if len(res.Content) == 0 {
				copy.Content = []mcp.Content{&mcp.TextContent{Text: text}}
			}
			return &copy, nil
		}
		return result, nil
	}
}

func contractObject(value any) (map[string]any, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(encoded, &obj) != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

func textContractSchema(value any) any {
	schema, ok := contractObject(value)
	if !ok {
		schema = map[string]any{"type": "object"}
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		schema["properties"] = props
	}
	props["content"] = map[string]any{"type": "string"}
	props["output"] = map[string]any{"type": "string", "description": "Compatibility alias for content."}
	required, _ := schema["required"].([]any)
	if !slices.Contains(required, any("content")) {
		required = append(required, "content")
	}
	schema["required"] = required
	return schema
}

func nativeTextTool(name string) bool {
	switch name {
	case "read_file", "write_file", "run_command", "git_status", "git_push", "git_commit", "git_log",
		"glob", "grep", "list_directory", "tree", "apply_patch", "update_plan", "clock_sleep",
		"get_agent_brief", "remember", "recall", "skills_read":
		return true
	}
	return false
}

func nativeToolAnnotations(name string) *mcp.ToolAnnotations {
	read, destructive, idempotent, open := false, false, false, false
	switch name {
	case "read_file", "git_status", "git_log", "glob", "grep", "list_directory", "tree", "view_image",
		"clock_curr_time", "clock_sleep", "get_environment", "get_project_doc", "get_agent_brief",
		"recall", "skills_list", "skills_read", "mcp_list_sources", "mcp_search_tools", "mcp_get_tool",
		"shared_mcp_routes", "shared_mcp_tools":
		read, idempotent = true, true
	case "export_host_file":
		read = true
	case "import_host_file", "import_chatgpt_file":
		open = true
	case "show_changes": // The default advances a task's review checkpoint.
	case "write_file", "update_plan", "remember":
		destructive, idempotent = true, true
	case "apply_patch", "git_commit":
		destructive = true
	case "exec_command", "write_stdin", "stop_command", "run_command", "git_push", "mcp_call_tool", "shared_mcp_call":
		destructive, open = true, true
	default:
		return nil
	}
	return &mcp.ToolAnnotations{ReadOnlyHint: read, DestructiveHint: &destructive, IdempotentHint: idempotent, OpenWorldHint: &open}
}
