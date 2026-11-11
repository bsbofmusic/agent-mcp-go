package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reviewRef(state, repo, pathspec string) string {
	key := sha256.Sum256([]byte(state + "\x00" + repo + "\x00" + pathspec))
	return fmt.Sprintf("refs/amg/review/%x/", key[:16])
}

// Capture the actual dirty workspace before the first mutating tool. These
// checkpoints are internal Git refs; the real index and branch are untouched.
func (a *App) captureProjectBaseline(ctx context.Context, req *mcp.CallToolRequest, scope, directory string) error {
	state, err := a.existingScopeStatePath(req, scope, "last-review.json")
	if err != nil {
		if err == errTaskScopeRequired {
			return nil
		}
		return err
	}
	cfg := a.config.Get()
	root := cfg.WorkRoot
	dir, err := resolveConfigPath(cfg, directory)
	if err != nil {
		return err
	}
	for {
		info, statErr := os.Stat(dir)
		if statErr == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || dir == root {
			return nil
		}
		dir = parent
	}
	top, err := a.parityGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	} // Non-Git workspaces still support file tools.
	repo := strings.TrimSpace(top.Output)
	if _, err := resolveConfigPath(cfg, repo); err != nil {
		return err
	}
	ref := reviewRef(state, repo, ".") + "project-open"
	if _, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{tree}"); err == nil {
		return nil
	}
	head, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		head = TextOut{}
	}
	tmp, err := os.MkdirTemp("", "amg-review-baseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tree, err := a.reviewSnapshot(ctx, repo, strings.TrimSpace(head.Output), ".", []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")})
	if err != nil {
		return fmt.Errorf("capture review baseline: %w", err)
	}
	_, err = a.parityGit(ctx, repo, "update-ref", ref, tree, strings.Repeat("0", len(tree)))
	return err
}

func (a *App) reviewLockKey(ctx context.Context, directory string) (string, error) {
	cfg := a.config.Get()
	dir, err := resolveConfigPath(cfg, directory)
	if err != nil {
		return "", err
	}
	for {
		if info, statErr := os.Stat(dir); statErr == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if top, err := a.parityGit(ctx, dir, "rev-parse", "--show-toplevel"); err == nil {
		repo := strings.TrimSpace(top.Output)
		return resolveConfigPath(cfg, repo)
	}
	return dir, nil
}

func (a *App) reviewLifecycleMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, request)
		}
		req, ok := request.(*mcp.CallToolRequest)
		if !ok || req.Params == nil {
			return next(ctx, method, request)
		}
		var args map[string]any
		if json.Unmarshal(req.Params.Arguments, &args) != nil {
			return next(ctx, method, request)
		}
		text := func(key string) string { s, _ := args[key].(string); return s }
		var directories []string
		switch req.Params.Name {
		case "write_file", "import_host_file", "import_chatgpt_file":
			if text("path") != "" {
				directories = []string{filepath.Dir(text("path"))}
			}
		case "transfer_prepare":
			if text("direction") == "upload" && text("path") != "" {
				directories = []string{filepath.Dir(text("path"))}
			}
		case "exec_command":
			directories = []string{text("workdir")}
		case "run_command":
			directories = []string{""}
		case "git_commit":
			directories = []string{text("directory")}
		case "apply_patch":
			if ops, err := readCodexPatch(text("input")); err == nil {
				for _, op := range ops {
					directories = append(directories, filepath.Dir(op.path))
					if op.moveTo != "" {
						directories = append(directories, filepath.Dir(op.moveTo))
					}
				}
			}
		default:
			return next(ctx, method, request)
		}
		logRequestPhase(ctx, "review_locate")
		lockDirectories := directories
		var keys []string
		for _, directory := range lockDirectories {
			key, err := a.reviewLockKey(ctx, directory)
			if err != nil {
				return nil, err
			}
			keys = append(keys, key)
		}
		logRequestPhase(ctx, "review_lock")
		release, err := a.projectGuards.acquire(ctx, keys)
		if err != nil {
			return nil, err
		}
		defer release()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		logRequestPhase(ctx, "review_baseline")
		for _, dir := range directories {
			if err := a.captureProjectBaseline(ctx, req, text("scope"), dir); err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Refusing mutation because the review baseline could not be captured: " + err.Error()}}}, nil
			}
		}
		return next(ctx, method, request)
	}
}
