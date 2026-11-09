package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func TestLive300MiBTransfer(t *testing.T) {
	if os.Getenv("AGENT_MCP_GO_LARGE_TRANSFER") != "1" {
		t.Skip("opt-in 300 MiB external network test")
	}
	endpoint := os.Getenv("AGENT_MCP_GO_ENDPOINT")
	if !strings.HasPrefix(endpoint, "https://") {
		t.Fatal("HTTPS endpoint required")
	}
	cfg, e := liveTestConfig(t)
	if e != nil {
		t.Fatal(e)
	}
	token, e := readBearerToken(cfg.TokenFile)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	httpClient := &http.Client{Transport: bearerTransport{token: string(token), underlying: http.DefaultTransport}, Timeout: 90 * time.Second}
	client := mcp.NewClient(&mcp.Implementation{Name: "large-transfer-audit", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	dst := filepath.Join(liveTransferDirectory(t, cfg), "payload.bin")
	prepare := func(direction string) TransferOutput {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "transfer_prepare", Arguments: map[string]any{"direction": direction, "path": dst}})
		if err != nil || result.IsError {
			t.Fatalf("prepare %s: %v", direction, err)
		}
		b, _ := json.Marshal(result.StructuredContent)
		var v TransferOutput
		if err = json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(v.URL, "https://") {
			t.Fatal("insecure transfer URL")
		}
		return v
	}
	const chunkSize = 15 * 1024 * 1024
	const total = 300 * 1024 * 1024
	chunk := bytes.Repeat([]byte{0x6a}, chunkSize)
	expected := sha256.New()
	for i := 0; i < total/chunkSize; i++ {
		_, _ = expected.Write(chunk)
	}
	expectedSHA := hex.EncodeToString(expected.Sum(nil))
	upload := prepare("upload")
	for offset := 0; offset < total; offset += chunkSize {
		req, _ := http.NewRequestWithContext(ctx, "PATCH", upload.URL, bytes.NewReader(chunk))
		req.Header.Set("Upload-Length", strconv.Itoa(total))
		req.Header.Set("Upload-Offset", strconv.Itoa(offset))
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("upload offset %d: %v", offset, err)
		}
		small, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("chunk offset %d: status=%d", offset, resp.StatusCode)
		}
		if offset+chunkSize == total && !strings.Contains(string(small), expectedSHA) {
			t.Fatal("server final SHA-256 mismatched")
		}
	}
	t.Log("300 MiB upload PASS")
	download := prepare("download")
	observed := sha256.New()
	for start := 0; start < total; start += chunkSize {
		end := start + chunkSize - 1
		req, _ := http.NewRequestWithContext(ctx, "GET", download.URL, nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("download offset %d: %v", start, err)
		}
		if resp.StatusCode != 206 {
			resp.Body.Close()
			t.Fatalf("range status %d", resp.StatusCode)
		}
		n, err := io.Copy(observed, resp.Body)
		resp.Body.Close()
		if err != nil || n != chunkSize {
			t.Fatalf("download segment size %d error %v", n, err)
		}
	}
	actual := hex.EncodeToString(observed.Sum(nil))
	if actual != expectedSHA {
		t.Fatalf("300MiB downloaded SHA mismatch")
	}
	t.Log("300 MiB download and SHA-256 PASS")
}
