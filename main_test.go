package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testApp(t *testing.T) (*App, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	token := strings.Repeat("x", 48)
	tokPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokPath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Listen: "127.0.0.1:3100", WorkRoot: dir, StateDir: filepath.Join(dir, "state"),
		SkillsRoot: filepath.Join(dir, "skills"), TokenFile: tokPath, PublicURL: "",
		MaxFileBytes: 300 * 1024 * 1024, MaxOutputBytes: 128 * 1024, CommandTimeoutSec: 60,
	}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	app, err := newApp(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app.handler())
	t.Cleanup(srv.Close)
	return app, srv, token
}
func TestRejectNoAuth(t *testing.T) {
	_, srv, _ := testApp(t)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader("{}"))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("want 401, got %d", res.StatusCode)
	}
}
func TestOriginCheck(t *testing.T) {
	_, srv, token := testApp(t)
	req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://evil.invalid")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("want 403, got %d", res.StatusCode)
	}
}
func TestTokenAndSettings(t *testing.T) {
	a, _, token := testApp(t)
	if tokenEqual("invalid", a.token) {
		t.Fatal("token unexpectedly matched")
	}
	if !tokenEqual(token, a.token) {
		t.Fatal("valid token rejected")
	}
	c, err := a.config.Update(350*1024*1024, 200*1024, 120)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxFileBytes != 350*1024*1024 {
		t.Fatal("settings not active")
	}
	b, err := os.ReadFile(a.config.path)
	if err != nil || !bytes.Contains(b, []byte("367001600")) {
		t.Fatal("settings not durable")
	}
	if _, err = a.config.Update(10*1024*1024, 1024, 3601); err == nil {
		t.Fatal("invalid settings accepted")
	}
}
func TestPathConfinement(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveWorkPath(root, "../../etc/shadow"); err == nil {
		t.Fatal("path escape accepted")
	}
	p, err := resolveWorkPath(root, "notes/test.md")
	if err != nil || p != filepath.Join(root, "notes/test.md") {
		t.Fatal("valid path broken")
	}
}
func TestTransferTicket(t *testing.T) {
	app, srv, _ := testApp(t)
	dst := filepath.Join(app.config.Get().WorkRoot, "uploaded.bin")
	ticket, err := app.transfers.create("upload", dst, 3*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	src := bytes.Repeat([]byte("u"), 1024*1024)
	req, _ := http.NewRequest("PUT", srv.URL+"/transfer/"+ticket, bytes.NewReader(src))
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("upload HTTP %d", res.StatusCode)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(src, got) {
		t.Fatal("upload bytes mismatched")
	}
	req, _ = http.NewRequest("PUT", srv.URL+"/transfer/"+ticket, bytes.NewReader(src))
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal("upload ticket reused")
	}
	id, err := app.transfers.create("download", dst, 3*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/transfer/"+id, nil)
	req.Header.Set("Range", "bytes=0-99")
	res, err = srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 206 || len(got) != 100 {
		t.Fatalf("range response: status=%d len=%d", res.StatusCode, len(got))
	}
}
func TestCommandSession(t *testing.T) {
	app, _, _ := testApp(t)
	s, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: "printf command-ok", YieldTimeMS: 5000})
	if err != nil {
		t.Fatal(err)
	}
	out := s.wait(3000)
	if !out.Finished || !strings.Contains(out.Output, "command-ok") || out.ExitCode != 0 {
		t.Fatalf("command failed %+v", out)
	}
	if _, err := app.sessions.start(app.config.Get(), CommandInput{Cmd: "echo err", Workdir: "../../etc"}); err == nil {
		t.Fatal("command escaped work root")
	}
}

func TestChunkedUploadAndConflict(t *testing.T) {
	app, srv, _ := testApp(t)
	dst := filepath.Join(app.config.Get().WorkRoot, "chunked.bin")
	id, err := app.transfers.create("upload", dst, 2*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	writeChunk := func(offset int64, data []byte) int {
		t.Helper()
		req, _ := http.NewRequest("PATCH", srv.URL+"/transfer/"+id, bytes.NewReader(data))
		req.Header.Set("Upload-Offset", fmt.Sprint(offset))
		req.Header.Set("Upload-Length", fmt.Sprint(2*1024*1024))
		res, er := srv.Client().Do(req)
		if er != nil {
			t.Fatal(er)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	part1 := bytes.Repeat([]byte("a"), 1024*1024)
	part2 := bytes.Repeat([]byte("b"), 1024*1024)
	if status := writeChunk(0, part1); status != 200 {
		t.Fatalf("first chunk status %d", status)
	}
	if status := writeChunk(0, part2); status != 409 {
		t.Fatalf("expected conflict 409, got %d", status)
	}
	if status := writeChunk(int64(len(part1)), part2); status != 200 {
		t.Fatalf("second chunk status %d", status)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, append(part1, part2...)) {
		t.Fatal("chunked bytes differ")
	}
	if status := writeChunk(2*1024*1024, part2); status != 404 {
		t.Fatalf("reused chunked ticket accepted: %d", status)
	}
}

func TestUploadsFailClosedUnlessOverwriteExplicit(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			app, srv, _ := testApp(t)
			dst := filepath.Join(app.config.Get().WorkRoot, "keep.txt")
			if err := os.WriteFile(dst, []byte("ORIGINAL"), 0600); err != nil {
				t.Fatal(err)
			}
			var id string
			var err error
			id, err = app.transfers.create("upload", dst, 1024)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(method, srv.URL+"/transfer/"+id, bytes.NewBufferString("NEW"))
			if method == "PATCH" {
				req.Header.Set("Upload-Offset", "0")
				req.Header.Set("Upload-Length", "3")
			}
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusConflict {
				t.Fatalf("wanted conflict got %d", res.StatusCode)
			}
			data, err := os.ReadFile(dst)
			if err != nil || string(data) != "ORIGINAL" {
				t.Fatalf("existing content overwritten %q %v", data, err)
			}
			if method == "PATCH" {
				app.transfers.forget(id)
			}
			id, err = app.transfers.createWithOverwrite("upload", dst, 1024, true)
			if err != nil {
				t.Fatal(err)
			}
			req, _ = http.NewRequest(method, srv.URL+"/transfer/"+id, bytes.NewBufferString("NEW"))
			if method == "PATCH" {
				req.Header.Set("Upload-Offset", "0")
				req.Header.Set("Upload-Length", "3")
			}
			res, err = srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("explicit overwrite failed %d", res.StatusCode)
			}
			data, err = os.ReadFile(dst)
			if err != nil || string(data) != "NEW" {
				t.Fatalf("explicit overwrite did not work %q %v", data, err)
			}
		})
	}
}
