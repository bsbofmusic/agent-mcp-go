package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errTaskScopeRequired = errors.New("task state requires a ChatGPT conversation, a stateful MCP session, or an explicit scope; use a stable scope to resume across reconnects")

// ephemeralSessionMarker marks a scope directory that belongs to an anonymous
// MCP session instead of a durable explicit or ChatGPT scope. Only marked
// directories may ever be removed by the stale-state cleanup.
const ephemeralSessionMarker = ".ephemeral-mcp-session"

// scopeSubdirName is the single parent directory of all per-scope state.
const scopeSubdirName = "scopes"

// ephemeralSessionTTL is how long task state of an anonymous MCP session is
// kept. Durable scopes (explicit scope, ChatGPT conversation) are never
// removed by age.
const ephemeralSessionTTL = 30 * 24 * time.Hour

// scopeStateFiles is the closed set of state files a scope directory may hold.
var scopeStateFiles = map[string]bool{"plan.json": true, "notes.json": true, "last-review.json": true}

func chatGPTConversationID(req *mcp.CallToolRequest) string {
	if req != nil && req.Params != nil {
		if id, ok := req.Params.Meta["openai/session"].(string); ok && strings.TrimSpace(id) != "" {
			return id
		}
	}
	return ""
}

// resolveScopeKey maps a tool call to its state namespace. Explicit scopes
// and ChatGPT conversations are durable; an anonymous MCP session scope is
// ephemeral and may be cleaned up after ephemeralSessionTTL.
func resolveScopeKey(req *mcp.CallToolRequest, explicit string) (string, bool, error) {
	scope := strings.TrimSpace(explicit)
	namespace, maxLength := "amg-scope-v1:", 128
	ephemeral := false
	if scope == "" {
		if id := chatGPTConversationID(req); id != "" {
			// ChatGPT sends a conversation ID on tool calls, not initialize.
			// Keep it in its own namespace and never use it for authorization.
			scope, namespace, maxLength = id, "amg-chatgpt-scope-v1:", 512
		}
	}
	if scope == "" && req != nil && req.Session != nil {
		// Session scratch must not collide with an explicit task of the same
		// name. Never read or migrate the old shared-namespace records here.
		scope, namespace, ephemeral = req.Session.ID(), "amg-mcp-session-v1:", true
	}
	if scope == "" {
		return "", false, errTaskScopeRequired
	}
	if len(scope) > maxLength || strings.ContainsAny(scope, "\x00\r\n") {
		return "", false, errors.New("invalid scope size or characters")
	}
	return namespace + scope, ephemeral, nil
}

// scopeDir resolves the on-disk directory of one scope key. Every state file
// of a scope lives directly in this directory, and the directory path is the
// single lock key that serializes all of them.
func scopeDir(stateDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(filepath.Clean(stateDir), scopeSubdirName, hex.EncodeToString(sum[:16]))
}

// withScopeLock resolves key's scope directory and runs fn while holding the
// per-scope-directory guard in a.stateGuards. Notes, plans, the ephemeral
// marker and the handover snapshot of one scope are therefore never observed
// or removed halfway through an update, while independent scopes stay fully
// parallel. A canceled caller releases its guard instead of blocking others.
func (a *App) withScopeLock(ctx context.Context, stateDir, key string, fn func(scopeDir string) error) error {
	dir := scopeDir(stateDir, key)
	release, err := a.stateGuards.acquire(ctx, []string{dir})
	if err != nil {
		return err
	}
	defer release()
	return fn(dir)
}

// withScopeState resolves the caller's scope and runs fn under its directory
// lock. create prepares the directory (and stamps the ephemeral marker) before
// fn; a read of an existing ephemeral scope refreshes its activity marker.
func (a *App) withScopeState(ctx context.Context, req *mcp.CallToolRequest, explicit string, create bool, fn func(scopeDir string, ephemeral bool) error) error {
	key, ephemeral, err := resolveScopeKey(req, explicit)
	if err != nil {
		return err
	}
	stateDir := a.config.Get().StateDir
	sweep := false
	err = a.withScopeLock(ctx, stateDir, key, func(dir string) error {
		if create {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			if ephemeral {
				if err := touchEphemeralMarker(dir); err != nil {
					return err
				}
			}
		} else if ephemeral {
			// A read-only tool call on an existing anonymous session scope is
			// activity too: refresh its marker so a read-heavy session keeps
			// its 30-day TTL. Best effort: a failed touch must not fail a read.
			if _, err := os.Stat(filepath.Join(dir, ephemeralSessionMarker)); err == nil {
				_ = touchEphemeralMarker(dir)
			}
		}
		if err := fn(dir, ephemeral); err != nil {
			return err
		}
		sweep = create && ephemeral
		return nil
	})
	if err != nil {
		return err
	}
	if sweep {
		a.maybeSweepStaleScopes()
	}
	return nil
}

// scopeStatePath prevents state for one MCP client session from silently
// overwriting another's plan, review baseline or task notes.
// Explicit scope can be reused across session reconnects (same Bearer owner).
// This is logical separation, NOT an authorization boundary.
func (a *App) scopeStatePath(req *mcp.CallToolRequest, explicit, filename string) (string, error) {
	return a.scopeStatePathMode(req, explicit, filename, true)
}

func (a *App) existingScopeStatePath(req *mcp.CallToolRequest, explicit, filename string) (string, error) {
	return a.scopeStatePathMode(req, explicit, filename, false)
}

func (a *App) scopeStatePathMode(req *mcp.CallToolRequest, explicit, filename string, create bool) (string, error) {
	if !scopeStateFiles[filename] {
		return "", errors.New("unknown state file")
	}
	var path string
	err := a.withScopeState(context.Background(), req, explicit, create, func(dir string, _ bool) error {
		path = filepath.Join(dir, filename)
		return nil
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// touchEphemeralMarker creates or refreshes the ephemeral session marker of a
// scope directory. The marker keeps the last activity time of the scope.
// O_CREATE without O_TRUNC never truncates an existing marker, so concurrent
// or repeated calls cannot observe a half-written file.
func touchEphemeralMarker(directory string) error {
	marker := filepath.Join(directory, ephemeralSessionMarker)
	now := time.Now()
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chtimes(marker, now, now)
}

// maybeSweepStaleScopes runs the stale scope cleanup at most once per hour so
// long-running daemons reclaim expired anonymous session state without a
// restart. The sweep never blocks the caller.
func (a *App) maybeSweepStaleScopes() {
	now := time.Now()
	last := a.scopeSweepAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < time.Hour {
		return
	}
	if !a.scopeSweepAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	stateDir := a.config.Get().StateDir
	go func() { _ = a.cleanupStaleScopes(context.Background(), stateDir, ephemeralSessionTTL, 10*time.Minute) }()
}

// cleanupStaleScopes removes task state that can no longer be reached:
// completely empty scope directories, and anonymous MCP session scopes that
// have been inactive for longer than maxAge. Durable scopes (explicit scope,
// ChatGPT conversation) carry no ephemeral marker and are never removed.
//
// Each candidate directory is inspected and removed under its own per-scope
// lock, so a live update can never race the sweep into a missing directory.
// An empty directory younger than emptyGrace is left alone because a scope
// directory may be created just before its first state file is written.
func (a *App) cleanupStaleScopes(ctx context.Context, stateDir string, maxAge time.Duration, emptyGrace time.Duration) error {
	scopesRoot := filepath.Join(filepath.Clean(stateDir), scopeSubdirName)
	entries, err := os.ReadDir(scopesRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := a.cleanupScopeEntry(ctx, filepath.Join(scopesRoot, entry.Name()), maxAge, emptyGrace); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cleanupScopeEntry inspects and, when stale, removes exactly one scope
// directory while holding its per-scope lock.
func (a *App) cleanupScopeEntry(ctx context.Context, dirPath string, maxAge, emptyGrace time.Duration) error {
	release, err := a.stateGuards.acquire(ctx, []string{dirPath})
	if err != nil {
		return err
	}
	defer release()
	// Re-stat after acquiring the lock: the directory may have disappeared
	// or appeared between ReadDir and the lock.
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !dirInfo.IsDir() {
		return nil
	}
	children, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(children) == 0 {
		// An empty scope directory holds no state at all. A freshly created
		// one may be waiting for its first state file, so honour emptyGrace.
		if emptyGrace > 0 && time.Since(dirInfo.ModTime()) < emptyGrace {
			return nil
		}
		if err := os.Remove(dirPath); err != nil && !os.IsNotExist(err) {
			if fresh, readErr := os.ReadDir(dirPath); readErr == nil && len(fresh) > 0 {
				// An external writer repopulated it after the check.
				return nil
			}
			return err
		}
		return nil
	}
	markerInfo, err := os.Stat(filepath.Join(dirPath, ephemeralSessionMarker))
	if err != nil {
		// Durable scope: never remove it, whatever its age.
		return nil
	}
	if time.Since(markerInfo.ModTime()) > maxAge {
		return os.RemoveAll(dirPath)
	}
	return nil
}
