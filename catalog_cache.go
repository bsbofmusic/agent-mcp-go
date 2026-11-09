package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cache TTLs are variables so tests can shorten them deterministically.
var (
	catalogCleanTTL   = 2 * time.Minute
	catalogPartialTTL = 15 * time.Second
)

const (
	catalogMaxConcurrentRoutes = 8
	catalogScanTimeout         = 20 * time.Second
	catalogRouteTimeout        = 10 * time.Second
)

type catalogEntry struct {
	SourceID    string
	Name        string
	Description string
	SearchText  string
	RawTool     json.RawMessage
}

type routeCatalog struct {
	mu          sync.Mutex
	signature   string
	expires     time.Time
	entries     []catalogEntry
	unavailable []string
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
	if routeSignature(sources) != a.catalog.signature || !time.Now().Before(a.catalog.expires) || !a.catalog.successful[source] {
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
	key := routeSignature(sources)
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		a.catalog.mu.Lock()
		if (!force || waited) && key == a.catalog.signature && time.Now().Before(a.catalog.expires) {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			unavailable := append([]string{}, a.catalog.unavailable...)
			a.catalog.mu.Unlock()
			return entries, unavailable, nil
		}
		if !force && key == a.catalog.signature && len(a.catalog.entries) > 0 {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			unavailable := append([]string{}, a.catalog.unavailable...)
			if a.catalog.refreshDone == nil {
				a.catalog.refreshDone = make(chan struct{})
				go func() {
					background, cancel := context.WithTimeout(context.Background(), catalogScanTimeout)
					defer cancel()
					a.refreshCatalog(background, sources, key, list)
				}()
			}
			a.catalog.mu.Unlock()
			return entries, unavailable, nil
		}
		if ongoing := a.catalog.refreshDone; ongoing != nil {
			a.catalog.mu.Unlock()
			select {
			case <-ongoing:
				waited = true
			case <-ctx.Done():
				return nil, nil, ctx.Err()
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
func (a *App) refreshCatalog(ctx context.Context, sources []UpstreamSource, key string, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, []string, error) {
	entries, unavailable, successful, err := a.scanCatalogDetails(ctx, sources, list)
	a.catalog.mu.Lock()
	if err == nil && (len(entries) > 0 || len(unavailable) == 0 || len(a.catalog.entries) == 0 || a.catalog.signature != key) {
		a.catalog.signature = key
		a.catalog.entries = entries
		a.catalog.unavailable = unavailable
		a.catalog.successful = successful
		ttl := catalogCleanTTL
		if len(unavailable) > 0 {
			ttl = catalogPartialTTL
		}
		a.catalog.expires = time.Now().Add(ttl)
	} else if err == nil && a.catalog.signature == key {
		a.catalog.unavailable = unavailable
		a.catalog.successful = successful
		a.catalog.expires = time.Now().Add(catalogPartialTTL)
	}
	close(a.catalog.refreshDone)
	a.catalog.refreshDone = nil
	a.catalog.mu.Unlock()
	if err != nil {
		return nil, unavailable, err
	}
	return append([]catalogEntry{}, entries...), append([]string{}, unavailable...), nil
}

func (a *App) scanCatalog(ctx context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	entries, unavailable, _, err := a.scanCatalogDetails(ctx, sources, list)
	return entries, len(unavailable), err
}

func (a *App) scanCatalogDetails(parent context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, []string, map[string]bool, error) {
	ctx, cancel := context.WithTimeout(parent, catalogScanTimeout)
	defer cancel()
	type found struct {
		source string
		rows   []catalogEntry
		err    error
	}
	results := make(chan found, len(sources))
	jobs := make(chan UpstreamSource)
	workers := len(sources)
	if workers > catalogMaxConcurrentRoutes {
		workers = catalogMaxConcurrentRoutes
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range jobs {
				step, stop := context.WithTimeout(ctx, catalogRouteTimeout)
				tools, err := list(step, src)
				stop()
				result := found{source: src.ID, err: err}
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
	successful := map[string]bool{}
	for result := range results {
		if result.err == nil {
			successful[result.source] = true
			entries = append(entries, result.rows...)
		}
	}
	var unavailable []string
	for _, src := range sources {
		if !successful[src.ID] {
			unavailable = append(unavailable, src.ID)
		}
	}
	sort.Strings(unavailable)
	if parent.Err() != nil && len(successful) == 0 {
		return nil, unavailable, successful, parent.Err()
	}
	return entries, unavailable, successful, nil
}
