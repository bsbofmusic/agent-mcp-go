package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadFileRejectsNonRegularAndRecovers(t *testing.T) {
	app, _, _ := testApp(t)
	root := app.config.Get().WorkRoot
	fifo := filepath.Join(root, "waiting.fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	// Keep a writer connected without sending data: a nonblocking open alone
	// does not prevent Go's pollable Scanner from waiting on this FIFO.
	writer, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	server := mcp.NewServer(&mcp.Implementation{Name: "read-file-boundary", Version: "1"}, nil)
	app.registerCore(server)
	client := contractClient(t, server)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	type result struct {
		value *mcp.CallToolResult
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, callErr := client.CallTool(ctx, &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": fifo}})
		done <- result{value, callErr}
	}()
	select {
	case got := <-done:
		if got.err != nil || got.value == nil || !got.value.IsError || !strings.Contains(toolText(got.value), "regular file") {
			t.Fatalf("FIFO must return an explicit regular-file error: result=%v error=%v", got.value, got.err)
		}
	case <-time.After(time.Second):
		// Release the blocked read before failing so the regression leaves no
		// handler, writer, or client request behind.
		writer.Close()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("read_file remained blocked after releasing the FIFO writer")
		}
		t.Fatal("read_file waited on a non-regular source")
	}
	path := filepath.Join(root, "normal.txt")
	if err := os.WriteFile(path, []byte("normal file still readable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": path}})
	if err != nil || got == nil || got.IsError || !strings.Contains(toolText(got), "normal file still readable") {
		t.Fatalf("regular read after rejection failed: result=%v error=%v", got, err)
	}
}
