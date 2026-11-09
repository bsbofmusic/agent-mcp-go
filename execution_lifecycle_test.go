package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A zombie has stopped executing. Only our direct children can be reaped by
// os/exec; grandchildren are reparented to the host's init after group death.
func lifecycleProcessLive(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	end := bytes.LastIndexByte(raw, ')')
	return end < 0 || len(raw) <= end+2 || raw[end+2] != 'Z'
}

func lifecycleWait(t *testing.T, limit time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestExecutionLifecycleInheritedPipes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process groups")
	}
	for _, route := range []string{"run_command", "gitOp", "gitWithEnv"} {
		for _, mode := range []string{"deadline", "cancel", "parent-exit"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				pidPath := filepath.Join(dir, "child.pid")
				t.Setenv("LIFECYCLE_CHILD_PID", pidPath)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				script := "#!/bin/sh\nsleep 30 &\nprintf '%s\\n' \"$!\" > \"$LIFECYCLE_CHILD_PID\"\n"
				if mode != "parent-exit" {
					script += "wait\n"
				}
				for _, name := range []string{"go", "git"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				app := &App{config: &ConfigStore{cfg: Config{WorkRoot: dir, CommandTimeoutSec: 5, MaxOutputBytes: 4096}}}
				ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					var err error
					switch route {
					case "run_command":
						_, failed, e := app.parityRunCommand(ctx, RunCommandInput{Command: "go", Timeout: 5000})
						err = e
						if err == nil && failed {
							err = fmt.Errorf("command failed")
						}
					case "gitOp":
						_, err = app.gitOp(ctx, "", "status")
					case "gitWithEnv":
						_, err = app.gitWithEnv(ctx, "", nil, "status")
					}
					done <- err
				}()
				pid := 0
				lifecycleWait(t, time.Second, func() bool {
					raw, _ := os.ReadFile(pidPath)
					pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
					return pid > 0
				}, "fixture did not launch descendant")
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				if mode == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Error("inherited open pipe unexpectedly reported success")
					}
				case <-time.After(900 * time.Millisecond):
					t.Error("command exceeded deadline while descendant retained output pipe")
					_ = syscall.Kill(pid, syscall.SIGKILL)
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Fatal("command did not reap after fixture cleanup")
					}
					return
				}
				lifecycleWait(t, time.Second, func() bool { return !lifecycleProcessLive(pid) }, "descendant survived command completion")
			})
		}
	}
}

func TestExecutionLifecycleSessionShutdown(t *testing.T) {
	manager := NewSessionManager()
	cfg := Config{WorkRoot: t.TempDir(), CommandTimeoutSec: 30, MaxOutputBytes: 4096}
	defer manager.shutdown()
	var sessions []*Session
	for i := 0; i < 3; i++ {
		s, err := manager.start(cfg, CommandInput{Cmd: "exec sleep 30"})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	begin := time.Now()
	if err := manager.shutdown(); err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > 2500*time.Millisecond {
		t.Fatal("session shutdown exceeded shared bound")
	}
	for _, s := range sessions {
		select {
		case <-s.done:
		default:
			t.Fatal("shutdown returned before session waiter finished")
		}
		if s.command.ProcessState == nil {
			t.Fatal("session process was not reaped")
		}
		if err := syscall.Kill(s.command.Process.Pid, 0); err != syscall.ESRCH {
			t.Fatalf("reaped direct child still exists: %v", err)
		}
	}
	if _, err := manager.start(cfg, CommandInput{Cmd: "printf unexpected"}); err == nil {
		t.Fatal("shutdown manager admitted another Shell")
	}
}

func TestExecutionLifecycleBinarySIGTERM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process groups")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "agent-mcp-go")
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer buildCancel()
	goBinary, err := executionBinary(Config{}, "go")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(buildCtx, goBinary, "build", "-buildvcs=false", "-trimpath", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("lifecycle-fixture-", 3)
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Listen: address, WorkRoot: dir, StateDir: filepath.Join(dir, "state"), SkillsRoot: filepath.Join(dir, "skills"), TokenFile: tokenPath, PublicURL: "http://" + address, CommandTimeoutSec: 5, MaxFileBytes: 1 << 20, MaxOutputBytes: 4096}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", configPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	pid := 0
	waited := false
	t.Cleanup(func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if !waited {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("fixture binary cleanup did not reap")
			}
		}
	})
	healthClient := &http.Client{Timeout: 100 * time.Millisecond}
	lifecycleWait(t, 5*time.Second, func() bool {
		res, err := healthClient.Get("http://" + address + "/healthz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	}, "fixture binary did not become healthy")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	httpClient := &http.Client{Transport: lifecycleAuthTransport{token: token}}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "lifecycle-fixture", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + address + "/mcp", HTTPClient: httpClient}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	readyCtx, readyCancel := context.WithTimeout(ctx, time.Second)
	defer readyCancel()
	result, err := client.CallTool(readyCtx, &mcp.CallToolParams{Name: "exec_command", Arguments: map[string]any{"cmd": `printf '%s\n' "$$"; exec sleep 30`, "yield_time_ms": 10, "timeout_seconds": 2}})
	if err != nil || result.IsError {
		t.Fatalf("start shell: %v %+v", err, result)
	}
	var command CommandOutput
	if err := json.Unmarshal([]byte(toolText(result)), &command); err != nil {
		t.Fatal(err)
	}
	output := command.Output
	for strings.TrimSpace(output) == "" && !command.Finished {
		if command.SessionID <= 0 {
			t.Fatalf("shell fixture missing session: %+v", command)
		}
		result, err = client.CallTool(readyCtx, &mcp.CallToolParams{Name: "write_stdin", Arguments: map[string]any{"session_id": command.SessionID, "chars": "", "yield_time_ms": 50}})
		if err != nil || result.IsError {
			t.Fatalf("read shell fixture PID: %v %+v", err, result)
		}
		if err := json.Unmarshal([]byte(toolText(result)), &command); err != nil {
			t.Fatal(err)
		}
		output += command.Output
	}
	pid, err = strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid <= 0 || command.Finished || command.ExitCode != 0 {
		t.Fatalf("shell fixture missing pid/session: %+v %v", command, err)
	}
	if !lifecycleProcessLive(pid) {
		t.Fatal("fixture shell exited before signal")
	}
	begin := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Errorf("SIGTERM did not exit cleanly: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("SIGTERM did not stop fixture binary within bound")
	}
	lifecycleWait(t, 2500*time.Millisecond, func() bool { return !lifecycleProcessLive(pid) }, "active shell survived binary SIGTERM beyond command timeout")
	t.Logf("real binary SIGTERM exit and shell disappearance: %s", time.Since(begin))
}

type lifecycleAuthTransport struct{ token string }

func (a lifecycleAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(clone)
}
