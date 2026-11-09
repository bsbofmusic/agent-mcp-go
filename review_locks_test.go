package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReviewProjectLocksIsolateAndCancel(t *testing.T) {
	var pool projectLockPool
	release, err := pool.acquire(t.Context(), []string{"repo-a"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := pool.acquire(t.Context(), []string{"repo-b"})
	if err != nil {
		t.Fatal("unrelated repository was blocked", err)
	}
	other()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		undo, err := pool.acquire(ctx, []string{"repo-a"})
		if undo != nil {
			undo()
		}
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked acquisition did not cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked by another project operation")
	}
	release()
	release()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.locks) != 0 {
		t.Fatalf("idle lock resources did not converge: %d", len(pool.locks))
	}
}

func TestReviewProjectLockMultiKeyCancellationReleasesEarlierKeys(t *testing.T) {
	var pool projectLockPool
	releaseB, err := pool.acquire(t.Context(), []string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if release, err := pool.acquire(ctx, []string{"b", "a", "a"}); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("wanted bounded wait: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	releaseA, err := pool.acquire(ctx2, []string{"a"})
	if err != nil {
		t.Fatal("earlier lock leaked after cancellation", err)
	}
	releaseA()
	releaseB()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.locks) != 0 {
		t.Fatal("lock lease leaked")
	}
}
