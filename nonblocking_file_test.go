package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNonblockingRootRegularFile(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("target.txt", []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := root.Lstat("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	file, opened, err := openRootRegularFile(root, "target.txt", expected)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || string(data) != "original\n" || !os.SameFile(expected, opened) {
		t.Fatalf("regular source changed: %q read=%v close=%v info=%v", data, readErr, closeErr, opened)
	}
	// Hold the old inode alive so the replacement cannot reuse its identity.
	if err := root.Rename("target.txt", "held.txt"); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile("target.txt", []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	file, opened, err = openRootRegularFile(root, "target.txt", expected)
	if err == nil {
		file.Close()
		t.Fatal("opened a replacement regular inode after inspection")
	}
	if file != nil || opened != nil {
		t.Fatal("failed open retained a descriptor or accepted replacement metadata")
	}
}

func TestNonblockingRootRegularFileRejectsFIFOReplacement(t *testing.T) {
	for _, writer := range []string{"absent", "connected"} {
		t.Run(writer, func(t *testing.T) {
			// Bound a regression to the blocking open in a child. On timeout,
			// CommandContext kills and Wait reaps it before TempDir cleanup.
			// Three seconds also allow the race runtime's exit delay.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNonblockingRootRegularFileFIFOProbe$", "-test.count=1")
			cmd.Env = append(os.Environ(), "AMG_NONBLOCK_FIFO_ROOT="+t.TempDir(), "AMG_NONBLOCK_FIFO_WRITER="+writer)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("regular->FIFO open blocked (writer=%s); child killed and reaped: %v", writer, ctx.Err())
			}
			if err != nil {
				t.Fatalf("FIFO replacement probe: %v\n%s", err, output)
			}
		})
	}
}

func TestNonblockingRootRegularFileFIFOProbe(t *testing.T) {
	path := os.Getenv("AMG_NONBLOCK_FIFO_ROOT")
	if path == "" {
		t.Skip("child-only FIFO replacement probe")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("target.txt", []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := root.Lstat("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("target.txt", "held.txt"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(path, "target.txt"), 0600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AMG_NONBLOCK_FIFO_WRITER") == "connected" {
		writer, err := root.OpenFile("target.txt", os.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
	}
	file, opened, err := openRootRegularFile(root, "target.txt", expected)
	if err == nil {
		file.Close()
		t.Fatal("accepted FIFO in place of inspected regular inode")
	}
	if file != nil || opened != nil {
		t.Fatal("rejected FIFO retained a descriptor or accepted metadata")
	}
	// Even a freshly inspected FIFO must be rejected by the actual FD check,
	// not merely by comparing it with the previous regular inode's identity.
	fifoInfo, err := root.Lstat("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	file, opened, err = openRootRegularFile(root, "target.txt", fifoInfo)
	if err == nil {
		file.Close()
		t.Fatal("opened descriptor was not checked for regular-file type")
	}
	if file != nil || opened != nil {
		t.Fatal("non-regular FD was not closed")
	}
}
