package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var errTaskScopeRequired = errors.New("task state requires a ChatGPT conversation, a stateful MCP session, or an explicit scope; use a stable scope to resume across reconnects")

func chatGPTConversationID(req *mcp.CallToolRequest) string {
	if req != nil && req.Params != nil {
		if id, ok := req.Params.Meta["openai/session"].(string); ok && strings.TrimSpace(id) != "" {
			return id
		}
	}
	return ""
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
	scope := strings.TrimSpace(explicit)
	namespace, maxLength := "amg-scope-v1:", 128
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
		scope, namespace = req.Session.ID(), "amg-mcp-session-v1:"
	}
	if scope == "" {
		return "", errTaskScopeRequired
	}
	if len(scope) > maxLength || strings.ContainsAny(scope, "\x00\r\n") {
		return "", errors.New("invalid scope size or characters")
	}
	allowed := map[string]bool{"plan.json": true, "notes.json": true, "last-review.json": true}
	if !allowed[filename] {
		return "", errors.New("unknown state file")
	}
	sum := sha256.Sum256([]byte(namespace + scope))
	directory := filepath.Join(a.config.Get().StateDir, "scopes", hex.EncodeToString(sum[:16]))
	if create {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return "", err
		}
	}
	return filepath.Join(directory, filename), nil
}
