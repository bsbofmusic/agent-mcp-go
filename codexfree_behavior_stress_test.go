package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These are behavioral contracts from CodexFree v1.9.1, not just the same
// tool names. They intentionally run against disposable testApp state.
func TestCodexFreeRunCommandStatusAndErrorPropagation(t *testing.T) {
	app, _, _ := testApp(t)
	ok, failed, err := app.parityRunCommand(context.Background(), RunCommandInput{
		Command: "git", Args: []string{"--version"},
	})
	if err != nil || failed || !strings.Contains(ok.Output, "exit code: 0") {
		t.Fatalf("successful command must report exit code 0: %+v, %t, %v", ok, failed, err)
	}
	bad, failed, err := app.parityRunCommand(context.Background(), RunCommandInput{
		Command: "git", Args: []string{"-C", "/nonexistent-parity-dir", "status"},
	})
	if err != nil || !failed || !strings.Contains(bad.Output, "exit code: 128") {
		t.Fatalf("failed binary must report real exit code and MCP error: %+v, %t, %v", bad, failed, err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "run-command-fixture", Version: "1"}, nil)
	app.registerParity(server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), serverTransport)
	client := mcp.NewClient(&mcp.Implementation{Name: "run-command-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "run_command",
		Arguments: map[string]any{"command": "git", "args": []string{"-C", "/nonexistent-parity-dir", "status"}},
	})
	if err != nil || !result.IsError || !strings.Contains(toolText(result), "exit code: 128") {
		t.Fatalf("MCP transport concealed process exit failure: %v %+v", err, result)
	}
}

func TestReadFileProvidesCodexFreeStyleContinuation(t *testing.T) {
	app, _, _ := testApp(t)
	path := filepath.Join(app.config.Get().WorkRoot, "page.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "read-fixture", Version: "1"}, nil)
	app.registerCore(server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), serverTransport)
	client := mcp.NewClient(&mcp.Implementation{Name: "read-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	first, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"path": path, "limit": 2},
	})
	if err != nil || first == nil {
		t.Fatalf("first read failed: %v", err)
	}
	firstText, _ := first.StructuredContent.(map[string]any)["output"].(string)
	if err != nil || first.IsError || !strings.Contains(firstText, "offset=2") || !strings.Contains(firstText, "1\tone") {
		t.Fatalf("missing continuation hint: %v %v", err, first)
	}
	second, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "read_file", Arguments: map[string]any{"path": path, "offset": 2, "limit": 2},
	})
	if err != nil || second == nil {
		t.Fatalf("second read failed: %v", err)
	}
	secondText, _ := second.StructuredContent.(map[string]any)["output"].(string)
	if err != nil || second.IsError || !strings.Contains(secondText, "3\tthree") || strings.Contains(secondText, "offset=4") {
		t.Fatalf("incorrect final page: %v %v", err, second)
	}
}

func TestDefaultExecShellFollowsRustPlatformCapability(t *testing.T) {
	app, _, _ := testApp(t)
	path := defaultExecShell()
	_, bashErr := exec.LookPath("bash")
	if bashErr == nil && !strings.Contains(filepath.Base(path), "bash") {
		t.Fatalf("bash available but Go chooses different default than Rust: %q", path)
	}
	if bashErr != nil && path != "/bin/sh" {
		t.Fatalf("no Bash available, expected portable fallback: %q", path)
	}
	s, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: "printf 'BASH:%s' \"${BASH_VERSION:-none}\""})
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	got := s.wait(1500)
	if !got.Finished || got.ExitCode != 0 || !strings.Contains(got.Output, "BASH:") {
		t.Fatalf("default shell invocation failed: %+v", got)
	}
	if bashErr == nil && strings.Contains(got.Output, "BASH:none") {
		t.Fatalf("Bash is installed but default shell did not use it: %+v", got)
	}
}

func TestColdMCPClientBurstWithInvalidCallsAndRecovery(t *testing.T) {
	_, web, token := testApp(t)
	const clients = 16
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	latencies := make([]time.Duration, 0, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := time.Now()
			client := mcp.NewClient(&mcp.Implementation{Name: "cold-client-burst", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:             web.URL + "/mcp/stateless",
				HTTPClient:           &http.Client{Transport: testAuthenticatedHostTransport{token: token}},
				DisableStandaloneSSE: true,
			}, nil)
			if err != nil {
				t.Errorf("cold connect failed: %v", err)
				return
			}
			defer session.Close()
			bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{
				"path": "/nonexistent-amg-read-probe",
			}})
			if err == nil && !bad.IsError {
				t.Error("negative file read silently succeeded")
				return
			}
			good, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_environment", Arguments: map[string]any{}})
			if err != nil || good.IsError || !strings.Contains(toolText(good), "Linux") {
				t.Errorf("healthy tool failed after bad call: %v %s", err, toolText(good))
				return
			}
			mu.Lock()
			latencies = append(latencies, time.Since(now))
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(latencies) != clients {
		t.Fatalf("only %d/%d cold clients recovered after negative calls", len(latencies), clients)
	}
	// The one fixture directory is disposed by the Go test harness.
	if _, err := os.Stat(filepath.Join(os.TempDir(), "nonexistent-amg-read-probe")); err == nil {
		t.Fatal("stress test unexpectedly wrote outside its fixture")
	}
}
