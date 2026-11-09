package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestShellSessionIDsUnpredictableAndExactInJSON(t *testing.T) {
	app, _, _ := testApp(t)
	seen := map[int64]bool{}
	for i := 0; i < 5; i++ {
		s, err := app.sessions.start(app.config.Get(), CommandInput{
			Cmd: "sleep 1", YieldTimeMS: 250,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer s.stop()
		if s.ID < 1<<40 || s.ID >= 1<<52 || seen[s.ID] {
			t.Fatalf("session ID is guessable, duplicated, or exceeds JS exact integer range: %d", s.ID)
		}
		seen[s.ID] = true
		if got, err := app.sessions.get(s.ID); err != nil || got != s {
			t.Fatalf("valid capability handle failed: %v", err)
		}
	}
	if _, err := app.sessions.get(1); err == nil {
		t.Fatal("guessed sequential ID resolved")
	}
}

func TestShellConcurrentStartAndFinishAccounting(t *testing.T) {
	app, _, _ := testApp(t)
	const jobs = 16
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for try := 0; try < 10; try++ {
				s, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: "printf done"})
				if err != nil {
					if err.Error() == "too many active shell sessions" {
						time.Sleep(10 * time.Millisecond)
						continue
					}
					t.Errorf("unexpected start error: %v", err)
					return
				}
				if got := s.wait(2000); !got.Finished || got.ExitCode != 0 {
					t.Errorf("session did not complete cleanly: %+v", got)
				}
				return
			}
			t.Error("session capacity never recovered")
		}()
	}
	wg.Wait()
	// The lifecycle accounting previously read Session.finished while the
	// waiter wrote it under a different mutex; it is now lock-consistent.
	app.sessions.mu.Lock()
	defer app.sessions.mu.Unlock()
	for _, session := range app.sessions.sessions {
		if !session.isFinished() {
			t.Fatal("finished session still counted as live")
		}
	}
}

func TestResidentShellOutputIsIncrementalLikeCodexFree(t *testing.T) {
	app, _, _ := testApp(t)
	s, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: `printf 'FIRST'; read value; printf 'SECOND:%s' "$value"`})
	if err != nil {
		t.Fatal(err)
	}
	defer s.stop()
	first := s.wait(300)
	if !strings.Contains(first.Output, "FIRST") || first.Finished {
		t.Fatalf("missed first output chunk: %+v", first)
	}
	if err := s.write("demo\n"); err != nil {
		t.Fatal(err)
	}
	second := s.wait(2000)
	if !second.Finished || !strings.Contains(second.Output, "SECOND:demo") || strings.Contains(second.Output, "FIRST") {
		t.Fatalf("second output poll did not drain incrementally: %+v", second)
	}
	third := s.wait(20)
	if third.Output != "" {
		t.Fatalf("finished shell repeated already consumed output: %q", third.Output)
	}
}

func TestCodexFreeCommandErrorsAndCtrlCOverMCP(t *testing.T) {
	app, _, _ := testApp(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "exec-error-fixture", Version: "1"}, nil)
	app.registerCore(server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go server.Run(context.Background(), serverTransport)
	client := mcp.NewClient(&mcp.Implementation{Name: "exec-error-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"cmd": "exit 7", "yield_time_ms": 250},
	})
	if err != nil || !result.IsError {
		t.Fatalf("exit 7 must mark result as error like CodexFree: %v %+v", err, result)
	}
	ongoing, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_command", Arguments: map[string]any{"cmd": "sleep 8", "yield_time_ms": 250},
	})
	if err != nil || ongoing.IsError || !containsAll(toolText(ongoing), "session_id") {
		t.Fatalf("expected resumable process: %v %s", err, toolText(ongoing))
	}
	var info struct {
		SessionID int64 `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(toolText(ongoing)), &info); err != nil || info.SessionID == 0 {
		t.Fatalf("missing session handle in %s: %v", toolText(ongoing), err)
	}
	stop, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_stdin", Arguments: map[string]any{"session_id": info.SessionID, "chars": "\u0003", "yield_time_ms": 1000},
	})
	if err != nil || !containsAll(toolText(stop), "finished") {
		t.Fatalf("Ctrl-C failed to stop process group: %v %s", err, toolText(stop))
	}
}

type testAuthenticatedHostTransport struct{ token string }

func (x testAuthenticatedHostTransport) RoundTrip(original *http.Request) (*http.Response, error) {
	req := original.Clone(original.Context())
	req.Header = original.Header.Clone()
	req.Header.Set("Authorization", "Bearer "+x.token)
	req.Host = "127.0.0.1:3100"
	return http.DefaultTransport.RoundTrip(req)
}

func TestStatefulCrossClientCannotControlAnotherShell(t *testing.T) {
	_, web, token := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	connect := func(name string) *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             web.URL + "/mcp",
			HTTPClient:           &http.Client{Transport: testAuthenticatedHostTransport{token: token}},
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	one, two := connect("session-owner-one"), connect("session-owner-two")
	defer one.Close()
	defer two.Close()
	start, err := one.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{
		"cmd": `read answer; printf 'ANSWER:%s' "$answer"`, "yield_time_ms": 250,
	}})
	if err != nil || start.IsError {
		t.Fatalf("owner command failed: %v %s", err, toolText(start))
	}
	var info struct {
		SessionID int64 `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(toolText(start)), &info); err != nil || info.SessionID < 1<<40 {
		t.Fatalf("no secure numeric session ID: %v %s", err, toolText(start))
	}
	intruder, err := two.CallTool(ctx, &mcp.CallToolParams{Name: "write_stdin", Arguments: map[string]any{
		"session_id": info.SessionID, "chars": "intruder\n", "yield_time_ms": 250,
	}})
	if err == nil && !intruder.IsError {
		t.Fatal("a different stateful MCP client wrote into the owner's process")
	}
	accepted, err := one.CallTool(ctx, &mcp.CallToolParams{Name: "write_stdin", Arguments: map[string]any{
		"session_id": info.SessionID, "chars": "owner\n", "yield_time_ms": 1000,
	}})
	if err != nil || accepted.IsError || !strings.Contains(toolText(accepted), "ANSWER:owner") {
		t.Fatalf("owner could not resume its own shell: %v %s", err, toolText(accepted))
	}
}

func TestStatelessHTTPStillResumesOpaqueShellHandles(t *testing.T) {
	_, web, token := testApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "stateless-shell-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             web.URL + "/mcp/stateless",
		HTTPClient:           &http.Client{Transport: testAuthenticatedHostTransport{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	start, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{
		"cmd": `read value; printf 'RESUMED:%s' "$value"`, "yield_time_ms": 250,
	}})
	if err != nil || start.IsError {
		t.Fatalf("stateless shell start error: %v %s", err, toolText(start))
	}
	var info struct {
		SessionID int64 `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(toolText(start)), &info); err != nil || info.SessionID == 0 {
		t.Fatalf("stateless session did not return opaque handle: %v %s", err, toolText(start))
	}
	resumed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "write_stdin", Arguments: map[string]any{
		"session_id": info.SessionID, "chars": "works\n", "yield_time_ms": 1000,
	}})
	if err != nil || resumed.IsError || !strings.Contains(toolText(resumed), "RESUMED:works") {
		t.Fatalf("stateless session could not continue across POSTs: %v %s", err, toolText(resumed))
	}
}
