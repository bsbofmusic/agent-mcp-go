package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type attachmentRoundTrip func(*http.Request) (*http.Response, error)

func (f attachmentRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChatGPTAttachmentAtomicPublish(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	const fixture = "portable native attachment"
	in := ChatGPTImportInput{File: ChatGPTFile{
		DownloadURL: "https://files.example.com/fixture?sig=never-log-this",
		FileID:      "file_fixture", FileName: "ignored.txt",
	}, Path: "incoming/native.txt"}
	downloads := 0
	client := &http.Client{Transport: attachmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		downloads++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("attachment fetch inherited credentials")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fixture)),
			ContentLength: int64(len(fixture)), Header: http.Header{"Content-Type": {"text/plain"}}}, nil
	})}
	out, err := app.importChatGPTFile(context.Background(), in, client)
	if err != nil || out.Bytes != int64(len(fixture)) || out.Source != "openai_file" ||
		out.Sha256 != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(fixture))) {
		t.Fatalf("unexpected receipt %+v, %v", out, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, in.Path)); err != nil || string(got) != fixture {
		t.Fatalf("unexpected file %q, %v", got, err)
	}
	if _, err = app.importChatGPTFile(context.Background(), in, client); err == nil || downloads != 1 {
		t.Fatal("existing file did not fail before fetching")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	in.Path = "escape/out.txt"
	if _, err := app.importChatGPTFile(context.Background(), in, client); err == nil {
		t.Fatal("attachment wrote through a symlink outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("attachment escaped the workspace")
	}
	in.Path = "incoming/broken.txt"
	client.Transport = attachmentRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("short")), ContentLength: 100}, nil
	})
	if _, err := app.importChatGPTFile(context.Background(), in, client); err == nil {
		t.Fatal("truncated attachment accepted")
	}
	if entries, err := os.ReadDir(filepath.Join(root, "incoming")); err != nil || len(entries) != 1 {
		t.Fatalf("incomplete attachment left destination or staging file: %v %v", entries, err)
	}
	client.Transport = attachmentRoundTrip(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New(r.URL.String())
	})
	if _, err := app.importChatGPTFile(context.Background(), in, client); err == nil || strings.Contains(err.Error(), "never-log-this") {
		t.Fatal("signed URL leaked in tool error")
	}
}

func TestChatGPTAttachmentDownloadBoundary(t *testing.T) {
	for _, raw := range []string{"http://files.example.com/f", "https://user@files.example.com/f", "https://files.example.com:444/f",
		"https://localhost/f", "https://169.254.169.254/latest/meta-data", "https://[::ffff:10.0.0.1]/f", "https://100.64.0.1/f"} {
		u, err := url.Parse(raw)
		if err != nil || validateAttachmentURL(u) == nil {
			t.Errorf("unexpectedly permitted %s", raw)
		}
	}
	for _, raw := range []string{"192.168.1.3", "100.100.100.200", "198.18.0.1", "::1", "fd00::1", "64:ff9b::a9fe:a9fe"} {
		if publicDownloadIP(netip.MustParseAddr(raw)) {
			t.Errorf("unexpectedly allowed resolved address %s", raw)
		}
	}
	u, _ := url.Parse("https://files.example.com/f?sig=secret")
	if err := validateAttachmentURL(u); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "https://169.254.169.254/", nil)
	req.Header.Set("Referer", u.String())
	if attachmentHTTP.CheckRedirect(req, []*http.Request{{URL: u}}) == nil || req.Header.Get("Referer") != "" {
		t.Fatal("redirect bypassed network validation or retained a signed Referer")
	}
}
