package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Cache TTLs are variables so tests can shorten them deterministically.
var (
	catalogCleanTTL   = 2 * time.Minute
	catalogPartialTTL = 15 * time.Second
)

// Keep discovery bounded while avoiding four serial waves when a shared
// gateway contains many independent routes. No additional registry/process.
const catalogMaxConcurrentRoutes = 8

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
	failed      int
	refreshDone chan struct{}
}

func routeSignature(sources []UpstreamSource) string {
	var b strings.Builder
	for _, src := range sources {
		b.WriteString(src.ID)
		b.WriteByte(0)
		b.WriteString(src.URL)
		b.WriteByte(0)
	}
	return b.String()
}
func (a *App) getLiveCatalog(ctx context.Context, sources []UpstreamSource, force bool) ([]catalogEntry, int, error) {
	return a.getLiveCatalogWith(ctx, sources, force, a.listRouteTools)
}

// getLiveCatalogWith builds (or reuses) the ephemeral in-process tools index.
// The TTL starts when the scan *completes*, not when it begins: a partial scan
// can take longer than catalogPartialTTL (slow/unavailable routes), and
// anchoring expiry at the start would make a just-built catalog instantly stale.
func (a *App) getLiveCatalogWith(ctx context.Context, sources []UpstreamSource, force bool, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	key := routeSignature(sources)
	waited := false
	for {
		a.catalog.mu.Lock()
		if (!force || waited) && key == a.catalog.signature && time.Now().Before(a.catalog.expires) {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			failed := a.catalog.failed
			a.catalog.mu.Unlock()
			return entries, failed, nil
		}
		// Serve the previous verified catalog immediately while refreshing
		// expired entries in the background. Only identical route signatures
		// qualify: route additions/removals always require a fresh scan.
		// An explicit force_refresh remains synchronous.
		if !force && key == a.catalog.signature && len(a.catalog.entries) > 0 {
			entries := append([]catalogEntry{}, a.catalog.entries...)
			failed := a.catalog.failed
			if a.catalog.refreshDone == nil {
				a.catalog.refreshDone = make(chan struct{})
				go func() {
					background, cancel := context.WithTimeout(context.Background(), 45*time.Second)
					defer cancel()
					a.refreshCatalog(background, sources, key, list)
				}()
			}
			a.catalog.mu.Unlock()
			return entries, failed, nil
		}
		if ongoing := a.catalog.refreshDone; ongoing != nil {
			a.catalog.mu.Unlock()
			select {
			case <-ongoing:
				waited = true
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			}
			continue
		}
		// Only one scan runs at a time. Do not hold the mutex across slow
		// network operations: unrelated HTTP requests must remain responsive.
		a.catalog.refreshDone = make(chan struct{})
		a.catalog.mu.Unlock()
		break
	}
	return a.refreshCatalog(ctx, sources, key, list)
}

// One writer publishes a complete scan and releases all waiters, including
// when the scan fails or is cancelled. Network I/O never holds the mutex.
func (a *App) refreshCatalog(ctx context.Context, sources []UpstreamSource, key string, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	entries, failed, err := a.scanCatalog(ctx, sources, list)
	a.catalog.mu.Lock()
	if err == nil && (len(entries) > 0 || failed == 0 || len(a.catalog.entries) == 0 || a.catalog.signature != key) {
		a.catalog.signature = key
		a.catalog.entries = entries
		a.catalog.failed = failed
		ttl := catalogCleanTTL
		if failed > 0 {
			ttl = catalogPartialTTL
		}
		a.catalog.expires = time.Now().Add(ttl)
	} else if err == nil && a.catalog.signature == key {
		// A temporary total outage must not erase the last usable catalog.
		a.catalog.failed = failed
		a.catalog.expires = time.Now().Add(catalogPartialTTL)
	}
	close(a.catalog.refreshDone)
	a.catalog.refreshDone = nil
	a.catalog.mu.Unlock()
	if err != nil {
		return nil, failed, err
	}
	return append([]catalogEntry{}, entries...), failed, nil
}

func (a *App) scanCatalog(ctx context.Context, sources []UpstreamSource, list func(context.Context, UpstreamSource) ([]json.RawMessage, error)) ([]catalogEntry, int, error) {
	type found struct {
		rows []catalogEntry
		err  error
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
				step, cancel := context.WithTimeout(ctx, 10*time.Second)
				tools, err := list(step, src)
				cancel()
				if err != nil {
					results <- found{err: fmt.Errorf("route %s: %w", src.ID, err)}
					continue
				}
				var rows []catalogEntry
				for _, tool := range tools {
					name, desc := rawToolInfo(tool)
					if name == "" {
						continue
					}
					rows = append(rows, catalogEntry{
						SourceID: src.ID, Name: name, Description: desc,
						SearchText: desc + " " + string(tool), RawTool: tool,
					})
				}
				results <- found{rows: rows}
			}
		}()
	}
	go func() {
		defer close(results)
		for _, src := range sources {
			select {
			case jobs <- src:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				return
			}
		}
		close(jobs)
		wg.Wait()
	}()
	var all []catalogEntry
	failed := 0
	for result := range results {
		if result.err != nil {
			failed++
			continue
		}
		all = append(all, result.rows...)
	}
	if ctx.Err() != nil && len(all) == 0 {
		return nil, failed, ctx.Err()
	}
	return all, failed, nil
}
