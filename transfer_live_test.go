package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// liveTransferDirectory keeps disposable HTTP targets within work_root. Prefer
// the process temp directory when rooted resolution proves it is inside; the
// fallback is the configured work root itself. Cleanup uses the same parent.
func liveTransferDirectory(t testing.TB, cfg Config) string {
	t.Helper()
	root, err := os.OpenRoot(cfg.WorkRoot)
	if err != nil {
		t.Fatal(err)
	}
	parent := root
	base := filepath.Clean(cfg.WorkRoot)
	if rel, err := workspaceRelativePath(cfg, filepath.Clean(os.TempDir())); err == nil {
		if temp, err := root.OpenRoot(rel); err == nil {
			parent = temp
			base = filepath.Clean(os.TempDir())
			_ = root.Close()
		}
	}
	id, err := randomID()
	if err != nil {
		_ = parent.Close()
		t.Fatal(err)
	}
	name := ".agent-mcp-transfer-test-" + id
	if err = parent.Mkdir(name, 0700); err != nil {
		_ = parent.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// name is the exact random child created above, beneath the fixed parent.
		if err := parent.RemoveAll(name); err != nil {
			t.Errorf("remove transfer test directory: %v", err)
		}
		if err := parent.Close(); err != nil {
			t.Errorf("close transfer test parent: %v", err)
		}
	})
	return filepath.Join(base, name)
}

func TestLiveTransferAcrossReverseProxy(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_TRANSFER") != "1" {
		t.Skip("run on demand; writes a disposable test file")
	}
	cfg, err := liveTestConfig(t)
	if err != nil {
		t.Fatal(err)
	}
	token, err := readBearerToken(cfg.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 90 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url := os.Getenv("AGENT_MCP_GO_ENDPOINT")
	if url == "" {
		t.Fatal("AGENT_MCP_GO_ENDPOINT required")
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "go-transfer-live-check", Version: "1"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	dst := filepath.Join(liveTransferDirectory(t, cfg), "payload.bin")
	prepare := func(direction string) TransferOutput {
		t.Helper()
		result, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "transfer_prepare", Arguments: map[string]any{"direction": direction, "path": dst}})
		if e != nil || result.IsError {
			t.Fatalf("prepare failed %v", e)
		}
		raw, e := json.Marshal(result.StructuredContent)
		if e != nil {
			t.Fatal(e)
		}
		var tr TransferOutput
		if e = json.Unmarshal(raw, &tr); e != nil {
			t.Fatal(e)
		}
		if !strings.HasPrefix(tr.URL, "https://") {
			t.Fatalf("expected HTTPS ticket URL")
		}
		return tr
	}
	upload := prepare("upload")
	data := bytes.Repeat([]byte("z"), 2*1024*1024)
	for offset := 0; offset < len(data); offset += 1024 * 1024 {
		part := data[offset : offset+1024*1024]
		req, _ := http.NewRequestWithContext(ctx, "PATCH", upload.URL, bytes.NewReader(part))
		req.Header.Set("Upload-Offset", strconv.Itoa(offset))
		req.Header.Set("Upload-Length", strconv.Itoa(len(data)))
		res, e := httpClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("PATCH chunk status %d body %s", res.StatusCode, b)
		}
	}
	download := prepare("download")
	req, _ := http.NewRequestWithContext(ctx, "GET", download.URL, nil)
	req.Header.Set("Range", "bytes=10-109")
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 512))
	res.Body.Close()
	if err != nil || res.StatusCode != 206 || !bytes.Equal(b, data[10:110]) {
		t.Fatalf("download HTTP Range mismatch: code=%d bytes=%d", res.StatusCode, len(b))
	}
}
