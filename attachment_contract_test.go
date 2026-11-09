package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Validate the published JSON, after the SDK and all tools/list middleware.
// OpenAI fileParams may refer only to file objects or arrays of those objects.
// A portable object/string union must not be advertised as a host file input.
func assertAttachmentCatalog(t *testing.T, catalog map[string]map[string]any) {
	t.Helper()
	native, portable := catalog["import_chatgpt_file"], catalog["import_host_file"]
	if native == nil || portable == nil {
		t.Fatalf("attachment entry missing from wire catalog: native=%v portable=%v", native != nil, portable != nil)
	}
	nativeMeta, _ := native["_meta"].(map[string]any)
	if !reflect.DeepEqual(nativeMeta["openai/fileParams"], []any{"file"}) {
		t.Errorf("native alias lost its host file parameter: %v", nativeMeta)
	}
	for name, tool := range catalog {
		meta, _ := tool["_meta"].(map[string]any)
		params, present := meta["openai/fileParams"]
		if !present {
			continue
		}
		fields, ok := params.([]any)
		if !ok || len(fields) == 0 {
			t.Errorf("%s has invalid fileParams: %v", name, params)
			continue
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		for _, field := range fields {
			key, _ := field.(string)
			file, _ := properties[key].(map[string]any)
			if ref, _ := file["$ref"].(string); ref != "" {
				defs, _ := schema["$defs"].(map[string]any)
				file, _ = defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
			}
			if file["type"] == "array" {
				file, _ = file["items"].(map[string]any)
			}
			if file["type"] != "object" || file["oneOf"] != nil || file["anyOf"] != nil {
				t.Errorf("%s fileParams %q must resolve to a file object, got %v", name, key, file)
				continue
			}
			fileProperties, _ := file["properties"].(map[string]any)
			for _, property := range []string{"download_url", "file_id", "mime_type", "file_name"} {
				definition, _ := fileProperties[property].(map[string]any)
				if definition["type"] != "string" {
					t.Errorf("%s fileParams %q lacks string property %s", name, key, property)
				}
			}
			required, _ := file["required"].([]any)
			keys := make([]string, len(required))
			for i, value := range required {
				keys[i], _ = value.(string)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, []string{"download_url", "file_id"}) {
				t.Errorf("%s fileParams %q must require only download_url and file_id: %v", name, key, required)
			}
		}
	}
	portableMeta, _ := portable["_meta"].(map[string]any)
	if _, present := portableMeta["openai/fileParams"]; present {
		t.Error("portable object/staged union must not advertise host fileParams")
	}
}

func TestAttachmentNativeOnlyInput(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, StateDir: state, MaxFileBytes: 1 << 20}}}
	if err := os.MkdirAll(filepath.Join(state, "staged"), 0700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(state, "staged", "fixture")
	if err := os.WriteFile(staged, []byte("legacy staged bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	oldHTTP := attachmentHTTP
	downloads := 0
	attachmentHTTP = &http.Client{Transport: attachmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		downloads++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("native bytes")), ContentLength: 12, Header: http.Header{"Content-Type": {"text/plain"}}}, nil
	})}
	t.Cleanup(func() { attachmentHTTP = oldHTTP })
	server := mcp.NewServer(&mcp.Implementation{Name: "native-attachment-contract", Version: "1"}, nil)
	app.registerChatGPTImport(server)
	server.AddReceivingMiddleware(toolContractsMiddleware)
	client := contractClient(t, server)
	for _, file := range []any{
		"staged:fixture",
		map[string]any{"file_id": "native-id"},
		map[string]any{"downloadUrl": "https://files.example.com/authorized", "fileId": "native-id"},
	} {
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_chatgpt_file", Arguments: map[string]any{"file": file, "path": "native.txt"}})
		if err != nil || !result.IsError {
			t.Fatalf("native-only input accepted %v: %v %+v", file, err, result)
		}
	}
	if downloads != 0 {
		t.Fatalf("invalid native inputs triggered %d downloads", downloads)
	}
	if data, err := os.ReadFile(staged); err != nil || string(data) != "legacy staged bytes" {
		t.Fatalf("native alias consumed a legacy staged file: %q %v", data, err)
	}
	// The host is allowed to omit mime_type and file_name at runtime.
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_chatgpt_file", Arguments: map[string]any{
		"file": map[string]any{"download_url": "https://files.example.com/authorized?sig=fixture", "file_id": "native-id"}, "path": "native.txt",
	}})
	if err != nil || result.IsError || downloads != 1 {
		t.Fatalf("required-fields-only native import failed: %v %+v downloads=%d", err, result, downloads)
	}
	receipt := contractJSON(t, result.StructuredContent)
	if receipt["source"] != "openai_file" || receipt["bytes"] != float64(12) || receipt["mimeType"] != "text/plain" {
		t.Fatalf("native receipt lost: %v", receipt)
	}
	if data, err := os.ReadFile(filepath.Join(root, "native.txt")); err != nil || string(data) != "native bytes" {
		t.Fatalf("native input was not published: %q %v", data, err)
	}
	assertContractOutput(t, client, "import_chatgpt_file", result)
}

func TestAttachmentCatalogActualWire(t *testing.T) {
	for _, endpoint := range []string{"/mcp", "/mcp/stateless"} {
		t.Run(endpoint, func(t *testing.T) {
			_, server, token := testApp(t)
			session := ""
			request := func(method, body string) (map[string]any, http.Header) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, method, server.URL+endpoint, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "127.0.0.1:3100"
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("MCP-Protocol-Version", "2025-06-18")
				if session != "" {
					req.Header.Set("Mcp-Session-Id", session)
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode == http.StatusAccepted || (method == http.MethodDelete && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent)) {
					return nil, resp.Header
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s returned HTTP %d", endpoint, resp.StatusCode)
				}
				var wire map[string]any
				if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wire); err != nil {
					t.Fatal(err)
				}
				if wire["error"] != nil {
					t.Fatalf("%s protocol error: %v", endpoint, wire["error"])
				}
				return wire, resp.Header
			}
			if endpoint == "/mcp" {
				_, headers := request(http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"attachment-contract","version":"1"}}}`)
				session = headers.Get("Mcp-Session-Id")
				if session == "" {
					t.Fatal("stateful initialization did not establish a session")
				}
				t.Cleanup(func() { request(http.MethodDelete, "") })
				request(http.MethodPost, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			}
			wire, headers := request(http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
			if endpoint == "/mcp/stateless" && headers.Get("Mcp-Session-Id") != "" {
				t.Fatal("stateless catalog unexpectedly established a session")
			}
			result, _ := wire["result"].(map[string]any)
			tools, _ := result["tools"].([]any)
			catalog := make(map[string]map[string]any)
			for _, entry := range tools {
				tool, _ := entry.(map[string]any)
				name, _ := tool["name"].(string)
				if name == "" || catalog[name] != nil {
					t.Fatalf("invalid or repeated tool name %q", name)
				}
				catalog[name] = tool
			}
			t.Logf("actual %s wire catalog: %d tools", endpoint, len(catalog))
			assertAttachmentCatalog(t, catalog)
		})
	}
}
