package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A slow client must not hold the global ticket registry lock. A separate
// upload should finish even while the first request body is still streaming.
func TestChunkedUploadSlowTicketDoesNotBlockOtherTickets(t *testing.T) {
	m := NewTransferManager()
	firstPath := filepath.Join(t.TempDir(), "slow.bin")
	secondPath := filepath.Join(t.TempDir(), "fast.bin")
	first, err := m.create("upload", firstPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.create("upload", secondPath, 100)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m.handler(nil))
	defer srv.Close()
	pr, pw := io.Pipe()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = pw.Close()
	}()
	written := make(chan struct{})
	go func() {
		_, _ = pw.Write([]byte("abcd"))
		close(written)
		<-release
		_, _ = pw.Write([]byte("efgh"))
		_ = pw.Close()
	}()
	request := func(ctx context.Context, id, off, total string, body io.Reader) *http.Request {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, srv.URL+"/transfer/"+id, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Upload-Offset", off)
		req.Header.Set("Upload-Length", total)
		return req
	}
	firstResult := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(request(context.Background(), first, "0", "8", pr))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = &statusError{resp.StatusCode}
			}
		}
		firstResult <- err
	}()
	select {
	case <-written: // First request is inside its slow body read.
	case <-time.After(3 * time.Second):
		t.Fatal("slow upload did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(request(ctx, second, "0", "3", bytes.NewBufferString("xyz")))
	if err != nil {
		t.Fatalf("independent upload blocked by slow ticket: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("independent upload returned %d", resp.StatusCode)
	}
	close(release)
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slow upload failed to finish")
	}
	for path, expected := range map[string]string{firstPath: "abcdefgh", secondPath: "xyz"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != expected {
			t.Fatalf("upload %s got %q, err %v", path, got, err)
		}
	}
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }
