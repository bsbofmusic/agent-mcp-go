package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCapturesDirtyWorkspaceBeforeMutation(t *testing.T) {
	testReviewDirtyBaseline(t, true, true)
}
func TestReviewCapturesGenericStatelessScope(t *testing.T) { testReviewDirtyBaseline(t, false, true) }
func TestReviewCapturesUnbornProject(t *testing.T)         { testReviewDirtyBaseline(t, false, false) }

func testReviewDirtyBaseline(t *testing.T, native, committed bool) {
	app, srv, token := testApp(t)
	repo := filepath.Join(app.config.Get().WorkRoot, "project")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	path := filepath.Join(repo, "file.txt")
	git("init", "-q", "--initial-branch=main")
	if err := os.WriteFile(path, []byte("committed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if committed {
		git("add", ".")
		git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	}
	if err := os.WriteFile(path, []byte("user-existing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	indexBefore := git("ls-files", "--stage")
	call := func(name string, arguments map[string]any) map[string]any {
		t.Helper()
		params := map[string]any{"name": name, "arguments": arguments}
		if native {
			params["_meta"] = map[string]any{"openai/session": "dirty-baseline-test"}
		} else {
			arguments["scope"] = "generic-task"
		}
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("POST", srv.URL+"/mcp/stateless", strings.NewReader(string(body)))
		req.Host = "127.0.0.1:3100"
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		response, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var message map[string]any
		if err := json.NewDecoder(response.Body).Decode(&message); err != nil {
			t.Fatal(err)
		}
		result, ok := message["result"].(map[string]any)
		if response.StatusCode != 200 || !ok || result["isError"] == true {
			t.Fatalf("%s: %v", name, message)
		}
		return result
	}
	call("write_file", map[string]any{"path": path, "content": "user-existing\nagent-new\n"})
	review := call("show_changes", map[string]any{"directory": repo, "since": "project_open", "advance": false})
	encoded, _ := json.Marshal(review)
	if strings.Contains(string(encoded), "-committed") || strings.Contains(string(encoded), "+user-existing") || !strings.Contains(string(encoded), "+agent-new") {
		t.Fatalf("review baseline includes pre-existing user edits: %s", encoded)
	}
	if indexAfter := git("ls-files", "--stage"); indexAfter != indexBefore {
		t.Fatal("review baseline changed the real Git index")
	}
}
