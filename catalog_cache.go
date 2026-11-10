package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Durations are variables so regression tests can scale real budgets down.
var (
	catalogCleanTTL     = 2 * time.Minute
	catalogPartialTTL   = 15 * time.Second
	catalogScanTimeout  = 45 * time.Second
	catalogRouteTimeout = 30 * time.Second
)

const catalogMaxConcurrentRoutes = 8

type catalogEntry struct {
	SourceID    string
	Name        string
	Description string
	SearchText  string
	RawTool     json.RawMessage
}

type CatalogSourceFailure struct {
	Source     string `json:"source"`
	Stage      string `json:"stage"`
	Kind       string `json:"kind"`
	DurationMS int64  `json:"durationMs"`
}

type catalogStatus struct {
	unavailable []string
	failures    []CatalogSourceFailure
	stale       bool
}

type routeCatalog struct {
	mu          sync.Mutex
	signature   string
	expires     time.Time
	entries     []catalogEntry
	status      catalogStatus
	successful  map[string]bool
	refreshDone chan struct{}
}

func routeSignature(sources []UpstreamSource) string {
	// Discovery ordering is immaterial; route identity and URL remain authoritative.
	keys := make([]string, 0, len(sources))
	for _, src := range sources {
		keys = append(keys, src.ID+"\x00"+src.URL+"\x00")
	}
	sort.Strings(keys)
	return strings.Join(keys, "")
}

func (a *App) getLiveCatalog(ctx context.Context, sources []UpstreamSource, force bool) ([]catalogEntry, int, error) {
	return a.getLiveCatalogWith(ctx, sources, force, a.listRouteTools)
}

func (a *App) getLiveCatalogWith(ctx context.Context, sources []UpstreamSource, force bool, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	entries, unavailable, err := a.getLiveCatalogDetails(ctx, sources, force, list)
	return entries, len(unavailable), err
}

// A successful empty directory is cacheable, unlike an unavailable route.
func (a *App) cachedRoute(sources []UpstreamSource, source string, force bool) ([]catalogEntry, bool) {
	if force {
		return nil, false
	}
	a.catalog.mu.Lock()
	defer a.catalog.mu.Unlock()
	if routeSignature(sources) != a.catalog.signature || !time.Now().Before(a.catalog.expires) || a.catalog.status.stale || !a.catalog.successful[source] {
		return nil, false
	}
	rows := []catalogEntry{}
	for _, entry := range a.catalog.entries {
		if entry.SourceID == source {
			rows = append(rows, entry)
		}
	}
	return rows, true
}

func (a *App) getLiveCatalogDetails(ctx context.Context, sources []UpstreamSource, force bool, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, []string, error) {
	entries, status, err := a.getCatalogSnapshot(ctx, sources, force, list)
	return entries, status.unavailable, err
}

func (a *App) getCatalogSnapshot(ctx context.Context, sources []UpstreamSource, force bool, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, catalogStatus, error) {
	key := routeSignature(sources)
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, catalogStatus{}, err
		}
		a.catalog.mu.Lock()
		if (!force || waited) && key == a.catalog.signature && time.Now().Before(a.catalog.expires) {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			status := a.catalog.status
			a.catalog.mu.Unlock()
			return entries, status, nil
		}
		if !force && key == a.catalog.signature && len(a.catalog.entries) > 0 {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			status := a.catalog.status
			status.stale = true
			if a.catalog.refreshDone == nil {
				a.catalog.refreshDone = make(chan struct{})
				go func() {
					background, cancel := context.WithTimeout(context.Background(), catalogScanTimeout)
					defer cancel()
					a.refreshCatalog(background, sources, key, list)
				}()
			}
			a.catalog.mu.Unlock()
			return entries, status, nil
		}
		if ongoing := a.catalog.refreshDone; ongoing != nil {
			a.catalog.mu.Unlock()
			select {
			case <-ongoing:
				waited = true
			case <-ctx.Done():
				return nil, catalogStatus{}, ctx.Err()
			}
			continue
		}
		a.catalog.refreshDone = make(chan struct{})
		a.catalog.mu.Unlock()
		break
	}
	return a.refreshCatalog(ctx, sources, key, list)
}

// One writer publishes a completed scan and releases all waiters. Failed scans
// keep the last useful rows, but never mark their routes as newly verified.
func (a *App) refreshCatalog(ctx context.Context, sources []UpstreamSource, key string, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, catalogStatus, error) {
	entries, status, successful, err := a.scanCatalogDetails(ctx, sources, list)
	a.catalog.mu.Lock()
	if err == nil {
		if len(successful) > 0 || len(status.unavailable) == 0 || len(a.catalog.entries) == 0 || a.catalog.signature != key {
			a.catalog.signature = key
			a.catalog.entries = entries
		} else {
			status.stale = true
		}
		a.catalog.status = status
		a.catalog.successful = successful
		ttl := catalogCleanTTL
		if len(status.unavailable) > 0 {
			ttl = catalogPartialTTL
		}
		a.catalog.expires = time.Now().Add(ttl)
	}
	close(a.catalog.refreshDone)
	a.catalog.refreshDone = nil
	rows := append([]catalogEntry{}, a.catalog.entries...)
	a.catalog.mu.Unlock()
	if err != nil {
		return nil, status, err
	}
	return rows, status, nil
}

func (a *App) scanCatalog(ctx context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	entries, status, _, err := a.scanCatalogDetails(ctx, sources, list)
	return entries, len(status.unavailable), err
}

func (a *App) scanCatalogDetails(parent context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, catalogStatus, map[string]bool, error) {
	ctx, cancel := context.WithTimeout(parent, catalogScanTimeout)
	defer cancel()
	type found struct {
		source  string
		rows    []catalogEntry
		err     error
		elapsed time.Duration
	}
	results := make(chan found, len(sources))
	jobs := make(chan UpstreamSource)
	workers := len(sources)
	if workers > catalogMaxConcurrentRoutes {
		workers = catalogMaxConcurrentRoutes
	}
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				step, stop := context.WithTimeout(ctx, catalogRouteTimeout)
				at := time.Now()
				tools, err := list(step, src)
				stop()
				result := found{source: src.ID, err: err, elapsed: time.Since(at)}
				if err == nil {
					for _, tool := range tools {
						name, desc := rawToolInfo(tool)
						if name != "" {
							result.rows = append(result.rows, catalogEntry{SourceID: src.ID, Name: name, Description: desc, SearchText: desc + " " + string(tool), RawTool: tool})
						}
					}
				}
				results <- result
			}
		}()
	}
	go func() {
		defer close(results)
		defer wg.Wait()
		defer close(jobs)
		for _, src := range sources {
			if ctx.Err() != nil {
				return
			}
			select {
			case jobs <- src:
			case <-ctx.Done():
				return
			}
		}
	}()
	var entries []catalogEntry
	status := catalogStatus{}
	successful := map[string]bool{}
	seen := map[string]bool{}
	for result := range results {
		seen[result.source] = true
		if result.err == nil {
			successful[result.source] = true
			entries = append(entries, result.rows...)
		} else {
			stage := "catalog"
			var detail *catalogStageError
			if errors.As(result.err, &detail) {
				stage = detail.stage
			}
			status.failures = append(status.failures, CatalogSourceFailure{result.source, stage, catalogErrorKind(result.err), result.elapsed.Milliseconds()})
		}
	}
	for _, src := range sources {
		if !successful[src.ID] {
			status.unavailable = append(status.unavailable, src.ID)
		}
		if !seen[src.ID] {
			status.failures = append(status.failures, CatalogSourceFailure{src.ID, "scan_queue", catalogErrorKind(ctx.Err()), time.Since(started).Milliseconds()})
		}
	}
	sort.Strings(status.unavailable)
	sort.Slice(status.failures, func(i, j int) bool { return status.failures[i].Source < status.failures[j].Source })
	if parent.Err() != nil && len(successful) == 0 {
		return nil, status, successful, parent.Err()
	}
	return entries, status, successful, nil
}
