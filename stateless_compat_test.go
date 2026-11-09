package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestStatelessMCPCompatibilityAndAuth(t *testing.T) {
	_, srv, token := testApp(t)
	request := func(path, method, value string, auth bool) (int, map[string]any, http.Header) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(value))
		if err != nil {
			t.Fatal(err)
		}
		// Same explicitly allowlisted reverse-proxy Host as the live service.
		req.Host = "127.0.0.1:3100"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		var parsed map[string]any
		if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("decode %s %d: %v", path, resp.StatusCode, err)
			}
		}
		return resp.StatusCode, parsed, resp.Header
	}
	listBody := `{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`
	if status, _, _ := request("/mcp/stateless", "POST", listBody, false); status != http.StatusUnauthorized {
		t.Fatalf("stateless endpoint must reject anonymous requests, got %d", status)
	}
	if status, _, _ := request("/mcp/stateless", "GET", "", true); status != http.StatusMethodNotAllowed {
		t.Fatalf("stateless endpoint should not open a hanging SSE stream, got %d", status)
	}
	status, parsed, headers := request("/mcp/stateless", "POST", listBody, true)
	if status != http.StatusOK {
		t.Fatalf("stateless list status %d: %v", status, parsed)
	}
	if headers.Get("Mcp-Session-Id") != "" {
		t.Fatalf("stateless endpoint unexpectedly returned a session ID")
	}
	toolsResult, ok := parsed["result"].(map[string]any)
	if !ok {
		t.Fatalf("stateless list result missing: %v", parsed)
	}
	list, ok := toolsResult["tools"].([]any)
	if !ok || len(list) < 32 {
		t.Fatalf("expected full AMG tool catalogue, got %d", len(list))
	}
	catalog := make(map[string]map[string]any)
	for _, item := range list {
		if tool, ok := item.(map[string]any); ok {
			if name, ok := tool["name"].(string); ok {
				catalog[name] = tool
			}
		}
	}
	native, _ := json.Marshal(catalog["import_chatgpt_file"])
	if !containsAll(string(native), `"openai/fileParams":["file"]`, "download_url", "file_id", "mime_type", "file_name") ||
		catalog["import_host_file"] == nil {
		t.Fatal("native attachment capability replaced the portable tool or lost its file schema")
	}
	review, _ := json.Marshal(catalog["show_changes"])
	if !containsAll(string(review), reviewUIURI, "resourceUri") {
		t.Fatal("review did not advertise its MCP Apps resource")
	}
	// A second request without an initialization handshake or session header
	// must remain functional, unlike the original stateful endpoint.
	second, _, _ := request("/mcp/stateless", "POST", listBody, true)
	if second != http.StatusOK {
		t.Fatalf("stateless follow-up without session failed: %d", second)
	}
	status, parsed, _ = request("/mcp", "POST", listBody, true)
	if status != http.StatusOK {
		t.Fatalf("existing stateful protocol returned unexpected HTTP status %d", status)
	}
	if parsed["error"] == nil {
		t.Fatalf("stateful endpoint unexpectedly accepted tools/list before initialization")
	}
	call := func(tool, session, args string) map[string]any {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":%q,"arguments":%s,"_meta":{"openai/session":%q}}}`, tool, args, session)
		status, parsed, _ := request("/mcp/stateless", "POST", body, true)
		result, ok := parsed["result"].(map[string]any)
		if status != 200 || !ok || result["isError"] == true {
			t.Fatalf("%s failed: %d %v", tool, status, parsed)
		}
		return result
	}
	call("remember", "conversation-one", `{"key":"fixture","value":"restore-only-in-first-chat"}`)
	recovered, _ := json.Marshal(call("get_agent_brief", "conversation-one", `{}`))
	other, _ := json.Marshal(call("get_agent_brief", "conversation-two", `{}`))
	if !strings.Contains(string(recovered), "restore-only-in-first-chat") || strings.Contains(string(other), "restore-only-in-first-chat") {
		t.Fatal("ChatGPT task state did not survive a stateless request or crossed conversations")
	}
}
