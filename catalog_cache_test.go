package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetLiveCatalogCachesPartialFailureFromCompletion guards the regression
// where catalog expiry was anchored at scan start: with temporarily
// unavailable routes the partial-failure TTL (15s) is shorter than a full scan
// (~15-17s on the real stack), so the freshly built catalog expired before the
// next warm lookup. Cache lifetime must start when the scan finishes.
func TestGetLiveCatalogCachesPartialFailureFromCompletion(t *testing.T) {
	oldClean, oldPartial := catalogCleanTTL, catalogPartialTTL
	defer func() { catalogCleanTTL, catalogPartialTTL = oldClean, oldPartial }()
	catalogCleanTTL = time.Minute
	catalogPartialTTL = 25 * time.Millisecond

	app := &App{}
	var calls int32
	slow := func(_ context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		atomic.AddInt32(&calls, 1)
		if src.ID == "slow" {
			// The scan outlasts the partial TTL; old code set expiry from
			// scan start and therefore returned an already-stale catalog.
			time.Sleep(60 * time.Millisecond)
			return nil, errors.New("temporarily unavailable")
		}
		return []json.RawMessage{json.RawMessage(`{"name":"get_user_profile"}`)}, nil
	}
	sources := []UpstreamSource{
		{ID: "fast", URL: "http://fast.invalid/mcp"},
		{ID: "slow", URL: "http://slow.invalid/mcp"},
	}

	entries, failed, err := app.getLiveCatalogWith(context.Background(), sources, false, slow)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if failed != 1 || len(entries) != 1 {
		t.Fatalf("want 1 entry and 1 failed route, got entries=%d failed=%d", len(entries), failed)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("first scan should call each source once, calls=%d", got)
	}

	// Immediate warm lookup must be a cache hit even though the scan itself
	// was longer than catalogPartialTTL.
	start := time.Now()
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, slow); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}
	if took := time.Since(start); took > catalogPartialTTL/2 {
		t.Fatalf("warm lookup took %s, expected cache hit", took)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("warm lookup should not rescan, calls=%d", got)
	}

	// Once the TTL passes, the previous verified catalog is served without
	// blocking; a single background refresh updates it.
	time.Sleep(catalogPartialTTL + 20*time.Millisecond)
	start = time.Now()
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, slow); err != nil {
		t.Fatalf("post-TTL scan: %v", err)
	}
	if took := time.Since(start); took > 20*time.Millisecond {
		t.Fatalf("stale catalog should return without waiting for slow route: %s", took)
	}
	deadline := time.Now().Add(time.Second)
	for {
		app.catalog.mu.Lock()
		refreshing := app.catalog.refreshDone != nil
		app.catalog.mu.Unlock()
		if !refreshing && atomic.LoadInt32(&calls) == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background catalog refresh did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("post-TTL lookup should rescan, calls=%d", got)
	}

	// force_refresh bypasses the cache.
	if _, _, err := app.getLiveCatalogWith(context.Background(), sources, true, slow); err != nil {
		t.Fatalf("force refresh: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 6 {
		t.Fatalf("force refresh should rescan, calls=%d", got)
	}
}

func TestCatalogRefreshDoesNotHoldMutexAndCoalescesWaiters(t *testing.T) {
	app := &App{}
	sources := []UpstreamSource{{ID: "demo", URL: "http://example.invalid/mcp"}}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []json.RawMessage{json.RawMessage(`{"name":"tool_one"}`)}, nil
	}
	done := make(chan error, 1)
	go func() { _, _, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch); done <- err }()
	<-started
	if !app.catalog.mu.TryLock() {
		t.Fatal("cache mutex held for the duration of a slow backend scan")
	}
	app.catalog.mu.Unlock()
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := app.getLiveCatalogWith(waitCtx, sources, false, fetch); err != context.Canceled {
		t.Fatalf("cancelled waiting client did not exit promptly: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch)
	if err != nil || len(got) != 1 || calls.Load() != 1 {
		t.Fatalf("failed to share completed scan: err %v entries %d calls %d", err, len(got), calls.Load())
	}
}

func TestCatalogDiscoveryBoundedParallelism(t *testing.T) {
	app := &App{}
	var active, peak atomic.Int32
	sources := make([]UpstreamSource, 24)
	for i := range sources {
		sources[i] = UpstreamSource{ID: string(rune('a' + i)), URL: "http://example.invalid/mcp"}
	}
	fetch := func(ctx context.Context, _ UpstreamSource) ([]json.RawMessage, error) {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []json.RawMessage{json.RawMessage(`{"name":"healthy_tool"}`)}, nil
	}
	entries, failed, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch)
	if err != nil || failed != 0 || len(entries) != len(sources) {
		t.Fatalf("catalog scan incomplete: entries=%d failed=%d err=%v", len(entries), failed, err)
	}
	if got := peak.Load(); got != catalogMaxConcurrentRoutes {
		t.Fatalf("expected %d bounded concurrent route scans, got %d", catalogMaxConcurrentRoutes, got)
	}
}

func TestCatalogCancellationCountsUnscheduledSources(t *testing.T) {
	app := &App{}
	sources := make([]UpstreamSource, 24)
	for i := range sources {
		sources[i] = UpstreamSource{ID: string(rune('a' + i)), URL: "http://fixture.invalid/mcp"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	fetch := func(ctx context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		if src.ID == "a" {
			return []json.RawMessage{json.RawMessage(`{"name":"ready"}`)}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	entries, unavailable, err := app.getLiveCatalogDetails(ctx, sources, false, fetch)
	if err != nil || len(entries) != 1 || len(unavailable) != 23 {
		t.Fatalf("cancelled scan silently lost routes: entries=%d unavailable=%d err=%v", len(entries), len(unavailable), err)
	}
	if unavailable[0] != "b" || unavailable[22] != "x" {
		t.Fatalf("unexpected unavailable sources: %v", unavailable)
	}
}

func TestCatalogSourceCacheEmptyFailureForceAndSignature(t *testing.T) {
	app := &App{}
	sources := []UpstreamSource{{ID: "empty", URL: "http://empty.invalid/mcp"}, {ID: "failed", URL: "http://failed.invalid/mcp"}}
	fetch := func(_ context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		if src.ID == "failed" {
			return nil, errors.New("unavailable")
		}
		return []json.RawMessage{}, nil
	}
	if _, failed, err := app.getLiveCatalogWith(context.Background(), sources, false, fetch); err != nil || failed != 1 {
		t.Fatalf("initial scan: %d %v", failed, err)
	}
	if rows, ok := app.cachedRoute(sources, "empty", false); !ok || len(rows) != 0 {
		t.Fatal("successful empty catalog was not cached")
	}
	if _, ok := app.cachedRoute(sources, "failed", false); ok {
		t.Fatal("failed route treated as empty success")
	}
	if _, ok := app.cachedRoute(sources, "empty", true); ok {
		t.Fatal("force_refresh reused cache")
	}
	reordered := []UpstreamSource{sources[1], sources[0]}
	if _, ok := app.cachedRoute(reordered, "empty", false); !ok {
		t.Fatal("equivalent source ordering missed cache")
	}
	changed := append([]UpstreamSource{}, sources...)
	changed[0].URL += "/new"
	if _, ok := app.cachedRoute(changed, "empty", false); ok {
		t.Fatal("changed route URL reused cache")
	}
	app.catalog.mu.Lock()
	app.catalog.expires = time.Now().Add(-time.Second)
	app.catalog.mu.Unlock()
	if _, ok := app.cachedRoute(sources, "empty", false); ok {
		t.Fatal("expired source catalog reused")
	}
}

func TestCatalogSuccessfulEmptyRefreshRemovesOldToolsDuringPartialOutage(t *testing.T) {
	app := &App{}
	sources := []UpstreamSource{{ID: "a", URL: "http://a.invalid/mcp"}, {ID: "b", URL: "http://b.invalid/mcp"}}
	initial := func(_ context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		return []json.RawMessage{json.RawMessage(`{"name":"old_` + src.ID + `"}`)}, nil
	}
	if rows, failed, err := app.getLiveCatalogWith(context.Background(), sources, false, initial); err != nil || failed != 0 || len(rows) != 2 {
		t.Fatalf("initial catalog: rows=%d failed=%d err=%v", len(rows), failed, err)
	}
	refresh := func(_ context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		if src.ID == "b" {
			return nil, errors.New("temporarily unavailable")
		}
		return []json.RawMessage{}, nil
	}
	if rows, failed, err := app.getLiveCatalogWith(context.Background(), sources, true, refresh); err != nil || failed != 1 || len(rows) != 0 {
		t.Fatalf("partial refresh: rows=%d failed=%d err=%v", len(rows), failed, err)
	}
	if rows, ok := app.cachedRoute(sources, "a", false); !ok || len(rows) != 0 {
		t.Fatalf("successful empty source retained deleted tools: rows=%v cached=%v", rows, ok)
	}
	if _, ok := app.cachedRoute(sources, "b", false); ok {
		t.Fatal("failed source advertised a fresh catalog")
	}
	if rows, failed, err := app.getLiveCatalogWith(context.Background(), sources, false, refresh); err != nil || failed != 1 || len(rows) != 0 {
		t.Fatalf("warm partial catalog retained deleted tools: rows=%d failed=%d err=%v", len(rows), failed, err)
	}
}

func TestCatalogRouteChangeInvalidatesStaleAndOutageKeepsLastGood(t *testing.T) {
	app := &App{}
	one := []UpstreamSource{{ID: "one", URL: "http://one.invalid/mcp"}}
	two := append(append([]UpstreamSource{}, one...), UpstreamSource{ID: "two", URL: "http://two.invalid/mcp"})
	fetch := func(_ context.Context, src UpstreamSource) ([]json.RawMessage, error) {
		return []json.RawMessage{json.RawMessage(`{"name":"tool_` + src.ID + `"}`)}, nil
	}
	entries, failed, err := app.getLiveCatalogWith(context.Background(), one, false, fetch)
	if err != nil || failed != 0 || len(entries) != 1 {
		t.Fatalf("initial catalog: %d %d %v", len(entries), failed, err)
	}
	entries, failed, err = app.getLiveCatalogWith(context.Background(), two, false, fetch)
	if err != nil || failed != 0 || len(entries) != 2 {
		t.Fatalf("route addition served stale catalog: %d %d %v", len(entries), failed, err)
	}
	outage := func(_ context.Context, _ UpstreamSource) ([]json.RawMessage, error) {
		return nil, errors.New("upstream down")
	}
	_, failed, err = app.getLiveCatalogWith(context.Background(), two, true, outage)
	if err != nil || failed != 2 {
		t.Fatalf("forced outage not surfaced: %d %v", failed, err)
	}
	entries, _, err = app.getLiveCatalogWith(context.Background(), two, false, outage)
	if err != nil || len(entries) != 2 {
		t.Fatalf("last good catalog erased by outage: %d %v", len(entries), err)
	}
}
