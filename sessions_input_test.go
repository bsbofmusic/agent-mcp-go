package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child deliberately leaves stdin unread until the parent releases it.
// exec replaces the fixture shell, so cleanup has one directly waited process.
func TestSessionInputChildProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--amg-session-input-child" {
		return
	}
	dir := os.Args[len(os.Args)-1]
	fmt.Print("INPUT_READY\n")
	deadline := time.Now().Add(6 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(91)
		}
		time.Sleep(time.Millisecond)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil || os.WriteFile(filepath.Join(dir, "received"), data, 0600) != nil {
		os.Exit(92)
	}
	fmt.Print("INPUT_EOF\n")
	os.Exit(0)
}

func sessionInputFixture(t *testing.T) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manager := NewSessionManager()
	s, err := manager.start(Config{WorkRoot: dir, MaxOutputBytes: 4096, CommandTimeoutSec: 10}, CommandInput{
		Cmd: fmt.Sprintf("exec %q -test.run=^TestSessionInputChildProcess$ -- --amg-session-input-child %q", executable, dir),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.shutdown(); err != nil {
			t.Error(err)
		}
		select {
		case <-s.done:
			if err := syscall.Kill(s.command.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Errorf("fixture process not reaped: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("fixture process was not cleaned up")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		text, _ := s.output.Snapshot()
		if strings.Contains(text, "INPUT_READY") {
			return s, dir
		}
		if s.isFinished() || time.Now().After(deadline) {
			t.Fatalf("fixture did not become ready: %+v", s.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

type sessionInputResult struct {
	n   int
	err error
}

func TestSessionInputCancellationAndQueuedWrite(t *testing.T) {
	s, dir := sessionInputFixture(t)
	payload := strings.Repeat("x", 2<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan sessionInputResult, 1)
	go func() {
		n, err := s.writeContext(ctx, payload)
		first <- sessionInputResult{n, err}
	}()
	select {
	case result := <-first:
		t.Fatalf("large write should be blocked before fixture reads: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	queuedCtx, queuedCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer queuedCancel()
	queued := make(chan sessionInputResult, 1)
	go func() {
		n, err := s.writeContext(queuedCtx, "MUST_NOT_BE_SENT\n")
		queued <- sessionInputResult{n, err}
	}()
	release := func() {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var queuedResult sessionInputResult
	select {
	case queuedResult = <-queued:
	case <-time.After(500 * time.Millisecond):
		t.Error("request cancellation did not interrupt the queued stdin write")
		release() // Also makes the unfixed implementation bounded during RED.
		select {
		case queuedResult = <-queued:
		case <-time.After(2 * time.Second):
			s.stop()
			queuedResult = <-queued
		}
	}
	if queuedResult.n != 0 || !errors.Is(queuedResult.err, context.DeadlineExceeded) {
		t.Errorf("queued cancellation must write zero bytes: %+v", queuedResult)
	}
	cancel()
	var firstResult sessionInputResult
	select {
	case firstResult = <-first:
	case <-time.After(500 * time.Millisecond):
		t.Error("request cancellation did not interrupt backpressured stdin")
		release()
		select {
		case firstResult = <-first:
		case <-time.After(2 * time.Second):
			s.stop()
			firstResult = <-first
		}
	}
	if firstResult.n <= 0 || firstResult.n >= len(payload) || !errors.Is(firstResult.err, context.Canceled) {
		t.Errorf("blocked write must report its actual accepted prefix: %+v", firstResult)
	}
	if firstResult.err == nil || !strings.Contains(firstResult.err.Error(), "do not replay") || !strings.Contains(firstResult.err.Error(), fmt.Sprintf("%d/%d", firstResult.n, len(payload))) {
		t.Errorf("write error needs count and explicit replay warning: %v", firstResult.err)
	}
	if s.isFinished() {
		t.Fatal("canceling a request terminated its resident session")
	}
	release()
	// The canceled write's deadline must be gone, and no goroutine may replay
	// its unwritten suffix after this next request starts.
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer nextCancel()
	if n, err := s.writeContext(nextCtx, "NEXT\n"); err != nil || n != len("NEXT\n") {
		t.Fatalf("next request inherited cancellation/deadline: n=%d err=%v", n, err)
	}
	if err := s.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	out := s.wait(3000)
	if !out.Finished || out.ExitCode != 0 || !strings.Contains(out.Output, "INPUT_EOF") {
		t.Fatalf("EOF contract failed after cancellation: %+v", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "received"))
	if err != nil {
		t.Fatal(err)
	}
	want := payload[:firstResult.n] + "NEXT\n"
	if string(got) != want {
		t.Errorf("stdin was interleaved or replayed: received=%d expected=%d", len(got), len(want))
	}
}

func TestSessionInputPipeDeadlineCleanup(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close(); writer.Close() })
	s := &Session{stdin: writer, inputGate: make(chan struct{}, 1), done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan sessionInputResult, 1)
	go func() {
		n, err := s.writeContext(ctx, strings.Repeat("p", 2<<20))
		done <- sessionInputResult{n, err}
	}()
	var result sessionInputResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		writer.Close()
		result = <-done
		t.Fatal("OS pipe deadline did not unblock the writer")
	}
	if result.n <= 0 || result.n >= 2<<20 || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("OS pipe lost deadline or partial-byte count: %+v", result)
	}
	prefix := make([]byte, result.n)
	if _, err := io.ReadFull(reader, prefix); err != nil || strings.Trim(string(prefix), "p") != "" {
		t.Fatalf("reported prefix differs from actual pipe: %v", err)
	}
	canceled, cancelBefore := context.WithCancel(context.Background())
	cancelBefore()
	if n, err := s.writeContext(canceled, "NEVER"); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("already canceled request wrote input: n=%d err=%v", n, err)
	}
	// Exercise successful-write callback removal too: cancellation after a
	// returned write must never attach an expired deadline to a later request.
	for i := 0; i < 32; i++ {
		request, cancelRequest := context.WithCancel(context.Background())
		n, err := s.writeContext(request, "NEXT")
		cancelRequest()
		if n != 4 || err != nil {
			t.Fatalf("next request inherited a stale deadline: n=%d err=%v", n, err)
		}
		var next [4]byte
		if _, err := io.ReadFull(reader, next[:]); err != nil || string(next[:]) != "NEXT" {
			t.Fatalf("next request was replayed/interleaved: %q %v", next, err)
		}
	}
}

func TestSessionInputStopUnblocksWriter(t *testing.T) {
	s, _ := sessionInputFixture(t)
	done := make(chan sessionInputResult, 1)
	go func() {
		n, err := s.writeContext(context.Background(), strings.Repeat("s", 2<<20))
		done <- sessionInputResult{n, err}
	}()
	select {
	case result := <-done:
		t.Fatalf("fixture unexpectedly read stdin: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	s.stop()
	select {
	case result := <-done:
		if result.err == nil || result.n >= 2<<20 {
			t.Errorf("stopped session accepted full stdin: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop left a blocked writer goroutine")
	}
	if out := s.wait(2000); !out.Finished {
		t.Fatal("stop did not finish session")
	}
}

func TestSessionWaitContextCancellationPreservesOutput(t *testing.T) {
	s, _ := sessionInputFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	type result struct {
		out CommandOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.waitContext(ctx, 30000)
		done <- result{out, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("request cancellation did not interrupt session wait")
		s.stop() // Bounds the old wait during RED and collects its goroutine.
		got = <-done
	}
	if !errors.Is(got.err, context.DeadlineExceeded) {
		t.Errorf("wait lost request cancellation: %v", got.err)
	}
	if got.out.Output != "" || s.isFinished() {
		t.Errorf("canceled wait drained output or stopped session: %+v", got.out)
	}
	out := s.wait(1)
	if !strings.Contains(out.Output, "INPUT_READY") || out.Finished {
		t.Errorf("next poll lost output/session after canceled wait: %+v", out)
	}
}
