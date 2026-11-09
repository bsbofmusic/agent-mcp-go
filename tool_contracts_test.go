package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func contractClient(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "contract-client", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func contractJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func assertContractOutput(t *testing.T, cs *mcp.ClientSession, name string, result *mcp.CallToolResult) {
	t.Helper()
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != name {
			continue
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal([]byte(mustContractJSON(t, tool.OutputSchema)), &schema); err != nil {
			t.Fatal(err)
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := resolved.Validate(result.StructuredContent); err != nil {
			t.Fatalf("advertised %s schema rejected actual result: %v", name, err)
		}
		return
	}
	t.Fatalf("tool %s not listed", name)
}

func TestToolContractsTextAndErrors(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "read_file"}, func(_ context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, TextOut, error) {
		if in.Path == "absent" {
			return nil, TextOut{}, errors.New("file absent")
		}
		return nil, TextOut{"1\t{\"content\":\"literal file text\"}\n"}, nil
	})
	server.AddReceivingMiddleware(toolContractsMiddleware)
	cs := contractClient(t, server)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": "ok"}})
	if err != nil || res.IsError {
		t.Fatalf("read: %v, %+v", err, res)
	}
	assertContractOutput(t, cs, "read_file", res)
	obj := contractJSON(t, res.StructuredContent)
	want := "1\t{\"content\":\"literal file text\"}\n"
	if obj["content"] != want || obj["output"] != want || toolText(res) != want {
		t.Errorf("expected literal text with both structured aliases; got %s", mustContractJSON(t, res))
	}
	for _, args := range []map[string]any{{"path": "absent"}, {}, {"path": 7}} {
		bad, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: args})
		if err != nil || !bad.IsError || bad.StructuredContent != nil || toolText(bad) == "" {
			t.Errorf("execution/validation error disguised as success: %v %+v", err, bad)
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "missing", Arguments: map[string]any{}}); err == nil {
		t.Fatal("unknown tool protocol error suppressed")
	}
}

func mustContractJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestToolContractsListCopiesAndAnnotations(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"output": map[string]any{"type": "string"}}, "required": []any{"output"}, "additionalProperties": false}
	original := &mcp.ListToolsResult{Tools: []*mcp.Tool{
		{Name: "read_file", OutputSchema: schema, Meta: mcp.Meta{"keep": "metadata"}},
		{Name: "mcp_call_tool"}, {Name: "mcp_search_tools"}, {Name: "import_host_file"}, {Name: "export_host_file"},
	}}
	before := mustContractJSON(t, original)
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return original, nil }
	for range 2 {
		result, err := toolContractsMiddleware(next)(context.Background(), "tools/list", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := result.(*mcp.ListToolsResult)
		textSchema := contractJSON(t, got.Tools[0].OutputSchema)
		props := textSchema["properties"].(map[string]any)
		if props["content"] == nil || props["output"] == nil {
			t.Errorf("canonical output schema absent: %v", textSchema)
		}
		if got.Tools[0].Annotations == nil || !got.Tools[0].Annotations.ReadOnlyHint || got.Tools[0].Meta["keep"] != "metadata" {
			t.Errorf("read-only annotation/metadata lost: %+v", got.Tools[0])
		}
		for _, tc := range []struct {
			index                               int
			read, destructive, idempotent, open bool
		}{
			{1, false, true, false, true}, {2, true, false, true, false}, {3, false, false, false, true}, {4, true, false, false, false},
		} {
			a := got.Tools[tc.index].Annotations
			if a == nil || a.ReadOnlyHint != tc.read || a.DestructiveHint == nil || *a.DestructiveHint != tc.destructive || a.IdempotentHint != tc.idempotent || a.OpenWorldHint == nil || *a.OpenWorldHint != tc.open {
				t.Errorf("incorrect annotations for %s: %+v", got.Tools[tc.index].Name, a)
			}
		}
		if after := mustContractJSON(t, original); after != before {
			t.Fatalf("middleware mutated shared registry: before=%s after=%s", before, after)
		}
	}
}

func TestToolContractsShellChunks(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "shell-contract", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "exec_command"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, CommandOutput, error) {
		return &mcp.CallToolResult{IsError: true}, CommandOutput{Output: "failed", Finished: true, ExitCode: 7, WallTimeSeconds: 0.1}, nil
	})
	server.AddReceivingMiddleware(toolContractsMiddleware)
	cs := contractClient(t, server)
	var previous string
	for range 2 {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{}})
		if err != nil || !res.IsError {
			t.Fatalf("command failure suppressed: %v %+v", err, res)
		}
		assertContractOutput(t, cs, "exec_command", res)
		obj := contractJSON(t, res.StructuredContent)
		chunk, _ := obj["chunk_id"].(string)
		if !strings.HasPrefix(chunk, "chunk-") || chunk == previous || obj["output"] != "failed" || obj["exit_code"] != float64(7) {
			t.Errorf("missing unique Rust chunk receipt: %v", obj)
		}
		var content map[string]any
		if err := json.Unmarshal([]byte(toolText(res)), &content); err != nil || !reflect.DeepEqual(content, obj) {
			t.Errorf("shell text/structured receipts diverged: %v %v", err, res)
		}
		previous = chunk
	}
}

// Each route-discovery process owns only this disposable SDK server, never a real shared route.
func TestToolContractsRouteHelperProcess(t *testing.T) {
	if os.Getenv("AMG_CONTRACT_HELPER") != "1" {
		return
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "route-fixture", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "mcp_list_routes"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"routes": []any{map[string]any{"name": "fixture", "path": "/mcp/fixture", "url": os.Getenv("AMG_CONTRACT_UPSTREAM")}}}, nil
	})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestToolContractsDispatcherRoundTrip(t *testing.T) {
	upstream := mcp.NewServer(&mcp.Implementation{Name: "upstream-fixture", Version: "1"}, nil)
	wants := map[string]*mcp.CallToolResult{
		"object": {Content: []mcp.Content{&mcp.TextContent{Text: "literal upstream text"}, &mcp.ImageContent{Data: []byte{1, 2, 3}, MIMEType: "image/png"}, &mcp.ResourceLink{URI: "fixture://artifact", Name: "artifact"}}, StructuredContent: map[string]any{"output": "upstream field", "payload": float64(23)}, Meta: mcp.Meta{"fixture": "keep"}},
		"array":  {Content: []mcp.Content{&mcp.TextContent{Text: "array"}}, StructuredContent: []any{"one", float64(2)}, Meta: mcp.Meta{"fixture": "array"}},
		"error":  {Content: []mcp.Content{&mcp.TextContent{Text: "upstream execution failed"}}, StructuredContent: map[string]any{"reason": "fixture"}, Meta: mcp.Meta{"fixture": "error"}, IsError: true},
	}
	for name, want := range wants {
		upstream.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return want, nil })
	}
	web := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstream }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(web.Close)
	t.Setenv("AMG_CONTRACT_HELPER", "1")
	t.Setenv("AMG_CONTRACT_UPSTREAM", web.URL)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "route-helper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\"'\"'")+"' -test.run='^TestToolContractsRouteHelperProcess$'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	app := &App{config: &ConfigStore{cfg: Config{SharedMCPBin: wrapper}}}
	server := mcp.NewServer(&mcp.Implementation{Name: "dispatcher-contract", Version: "1"}, nil)
	app.registerMCPParity(server)
	server.AddReceivingMiddleware(toolContractsMiddleware)
	cs := contractClient(t, server)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "mcp_call_tool" && tool.OutputSchema != nil {
			t.Errorf("dispatcher advertised a fake empty output schema: %v", tool.OutputSchema)
		}
	}
	for name, want := range wants {
		t.Run(name, func(t *testing.T) {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "mcp_call_tool", Arguments: map[string]any{"source": "fixture", "tool": name}})
			if err != nil {
				t.Fatal(err)
			}
			if mustContractJSON(t, res.Content) != mustContractJSON(t, want.Content) || mustContractJSON(t, res.StructuredContent) != mustContractJSON(t, want.StructuredContent) || res.IsError != want.IsError {
				t.Errorf("upstream result changed: got %s want %s", mustContractJSON(t, res), mustContractJSON(t, want))
			}
			for key, value := range want.Meta {
				if !reflect.DeepEqual(res.Meta[key], value) {
					t.Errorf("upstream result metadata lost: %s=%v", key, res.Meta[key])
				}
			}
		})
	}
}

func TestToolContractsHostImportNestedAndLegacy(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{WorkRoot: root, StateDir: state, MaxFileBytes: 1 << 20}}}
	oldHTTP := attachmentHTTP
	downloads := 0
	attachmentHTTP = &http.Client{Transport: attachmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		downloads++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("attachment fetch inherited caller credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("native bytes")), ContentLength: 12, Header: http.Header{"Content-Type": {"text/plain"}}}, nil
	})}
	t.Cleanup(func() { attachmentHTTP = oldHTTP })
	server := mcp.NewServer(&mcp.Implementation{Name: "import-contract", Version: "1"}, nil)
	app.registerFileParity(server)
	server.AddReceivingMiddleware(toolContractsMiddleware)
	cs := contractClient(t, server)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "import_host_file" {
			continue
		}
		input := contractJSON(t, tool.InputSchema)
		defs, _ := input["$defs"].(map[string]any)
		fileDef, _ := defs["OpenAIFile"].(map[string]any)
		if fileDef["additionalProperties"] != false || !reflect.DeepEqual(fileDef["required"], []any{"download_url", "file_id"}) {
			t.Errorf("native file schema is not strict: %v", input)
		}
		properties := input["properties"].(map[string]any)
		file := properties["file"].(map[string]any)
		if options, _ := file["oneOf"].([]any); len(options) != 2 {
			t.Errorf("missing object/string compatibility union: %v", file)
		}
		if _, present := tool.Meta["openai/fileParams"]; present {
			t.Errorf("portable object/string union advertised a host file parameter: %v", tool.Meta)
		}
	}
	for i, file := range []any{
		map[string]any{"download_url": "https://files.example.com/a"},
		map[string]any{"download_url": "https://files.example.com/a", "file_id": ""},
		map[string]any{"download_url": "https://files.example.com/a", "file_id": "   "},
		map[string]any{"download_url": 7, "file_id": "id"},
		map[string]any{"download_url": "https://files.example.com/a", "file_id": "id", "extra": "reject"},
		map[string]any{"download_url": "https://files.example.com/a", "file_id": "id", "mime_type": nil},
		map[string]any{"download_url": "https://files.example.com/a", "file_id": "id", "file_name": "\n"},
		map[string]any{"download_url": "https://127.0.0.1/private", "file_id": "id"},
		map[string]any{"download_url": "http://files.example.com/a", "file_id": "id"},
		nil, 7, []any{"no"}, "file-private-id", "staged:../../escape",
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_host_file", Arguments: map[string]any{"file": file, "path": "incoming.txt", "scope": "contract-task"}})
		if err != nil || !res.IsError {
			t.Errorf("invalid nested file %d accepted: %v %+v", i, err, res)
		}
	}
	if downloads != 0 {
		t.Fatalf("invalid inputs triggered %d downloads", downloads)
	}
	file := map[string]any{"download_url": "https://files.example.com/a?sig=fixture", "file_id": "fixture-id", "file_name": "attached.txt", "mime_type": "text/plain"}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_host_file", Arguments: map[string]any{"file": file, "path": "incoming.txt", "scope": "contract-task"}})
	if err != nil || res.IsError {
		t.Fatalf("native object import failed: %v %+v", err, res)
	}
	if obj := contractJSON(t, res.StructuredContent); obj["source"] != "openai_file" || obj["mimeType"] != "text/plain" || obj["bytes"] != float64(12) {
		t.Errorf("native receipt lost: %v", obj)
	}
	if got, err := os.ReadFile(filepath.Join(root, "incoming.txt")); err != nil || string(got) != "native bytes" {
		t.Errorf("native bytes not published: %q %v", got, err)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_host_file", Arguments: map[string]any{"file": file, "path": "incoming.txt", "scope": "contract-task"}})
	if err != nil || !res.IsError || downloads != 1 {
		t.Errorf("native import overwrote or redownloaded destination: %v %+v downloads=%d", err, res, downloads)
	}
	if err := os.MkdirAll(filepath.Join(state, "staged"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "staged", "fixture"), []byte("legacy bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "import_host_file", Arguments: map[string]any{"file": "staged:fixture", "path": "legacy.txt"}})
	if err != nil || res.IsError || contractJSON(t, res.StructuredContent)["source"] != "staged_transfer" || downloads != 1 {
		t.Errorf("legacy staged import regressed: %v %+v", err, res)
	}
}

func TestToolContractsImageHasNoFakeStructuredOutput(t *testing.T) {
	root := t.TempDir()
	app := &App{config: &ConfigStore{cfg: Config{WorkRoot: root}}}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "test.png"), pngBytes.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "image-contract", Version: "1"}, nil)
	app.registerFileParity(server)
	server.AddReceivingMiddleware(toolContractsMiddleware)
	cs := contractClient(t, server)
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "view_image" && tool.OutputSchema != nil {
			t.Errorf("image advertised fake empty output schema: %v", tool.OutputSchema)
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "view_image", Arguments: map[string]any{"path": "test.png"}})
	if err != nil || res.IsError {
		t.Fatalf("image error: %v %+v", err, res)
	}
	if res.StructuredContent != nil || len(res.Content) != 1 {
		t.Errorf("image got fake structured output: %+v", res)
	}
	if img, ok := res.Content[0].(*mcp.ImageContent); !ok || img.MIMEType != "image/png" || !bytes.Equal(img.Data, pngBytes.Bytes()) {
		t.Errorf("image changed: %+v", res)
	}
}
