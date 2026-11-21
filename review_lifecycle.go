package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func reviewRef(state, repo, pathspec string) string {
	key := sha256.Sum256([]byte(state + "\x00" + repo + "\x00" + pathspec))
	return fmt.Sprintf("refs/amg/review/%x/", key[:16])
}

// reviewBaselineCooldownDefault is how long one repository stays on the cheap
// HEAD-only baseline path after a full snapshot timed out or was skipped.
const reviewBaselineCooldownDefault = 10 * time.Minute

// reviewBaselineCooldownState tracks repositories whose full review snapshot
// recently failed or was skipped. It is process-local on purpose: a restart may
// retry once, and the baseline ref itself stays authoritative afterwards.
type reviewBaselineCooldownState struct {
	mu    sync.Mutex
	repos map[string]time.Time
}

func (s *reviewBaselineCooldownState) active(repo string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.repos[repo]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(s.repos, repo)
	return false
}

func (s *reviewBaselineCooldownState) mark(repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repos == nil {
		s.repos = make(map[string]time.Time)
	}
	s.repos[repo] = time.Now().Add(reviewBaselineCooldownDefault)
}

// reviewBaselineSkipped reports whether repo falls under one of the configured
// absolute prefixes that opt out of full snapshots.
func reviewBaselineSkipped(cfg Config, repo string) bool {
	for _, prefix := range cfg.ReviewBaselineSkipPaths {
		if repo == prefix || strings.HasPrefix(repo, prefix+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// reviewSnapshotFailureReason classifies a failed workspace snapshot so the
// diagnostic log distinguishes a deadline from any other Git failure.
func reviewSnapshotFailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "snapshot"
}

// Capture the actual dirty workspace before the first mutating tool. These
// checkpoints are internal Git refs; the real index and branch are untouched.
//
// The snapshot is best effort. A large repository, an expired deadline or a
// configured skip path degrades the baseline to HEAD^{tree} and records a
// repository cooldown instead of blocking the mutation: a bounded tool call
// must never wait on an expensive `git add -A`.
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
	head, headErr := a.parityGit(ctx, repo, "rev-parse", "--verify", "HEAD^{tree}")
	if headErr != nil {
		head = TextOut{}
	}
	headTree := strings.TrimSpace(head.Output)

	// degrade records the cheap HEAD/empty-tree baseline and starts the
	// cooldown. Failures here only log: the pending mutation still proceeds.
	degrade := func(reason string, cause error) error {
		a.reviewBaselineCooldown.mark(repo)
		a.logReviewBaseline(repo, reason, cause)
		tree := headTree
		if tree == "" {
			tmp, tempErr := os.MkdirTemp("", "amg-review-empty-")
			if tempErr != nil {
				a.logReviewBaseline(repo, "empty_tree", tempErr)
				return nil
			}
			defer os.RemoveAll(tmp)
			env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
			if _, treeErr := a.gitWithEnv(ctx, repo, env, "read-tree", "--empty"); treeErr != nil {
				a.logReviewBaseline(repo, "empty_tree", treeErr)
				return nil
			}
			written, treeErr := a.gitWithEnv(ctx, repo, env, "write-tree")
			if treeErr != nil {
				a.logReviewBaseline(repo, "empty_tree", treeErr)
				return nil
			}
			tree = strings.TrimSpace(written.Output)
		}
		if tree == "" {
			return nil
		}
		if _, refErr := a.parityGit(ctx, repo, "update-ref", ref, tree, strings.Repeat("0", len(tree))); refErr != nil {
			a.logReviewBaseline(repo, "update_ref", refErr)
		}
		return nil
	}

	if reviewBaselineSkipped(cfg, repo) {
		return degrade("skip_paths", nil)
	}
	if a.reviewBaselineCooldown.active(repo) {
		return degrade("cooldown", nil)
	}
	tmp, err := os.MkdirTemp("", "amg-review-baseline-")
	if err != nil {
		return degrade("temp_dir", err)
	}
	defer os.RemoveAll(tmp)
	snapshotCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ReviewBaselineTimeoutSec)*time.Second)
	defer cancel()
	logRequestPhase(ctx, "review_snapshot")
	tree, err := a.reviewSnapshot(snapshotCtx, repo, headTree, ".", []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")})
	if err != nil {
		// os/exec reports a child killed by the deadline as an exit error rather
		// than context.DeadlineExceeded, so consult the snapshot context before
		// classifying; the wrapped cause keeps the deadline visible in the log.
		reason := reviewSnapshotFailureReason(err)
		if reason != "timeout" && errors.Is(snapshotCtx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
		if reason == "timeout" {
			return degrade(reason, fmt.Errorf("deadline exceeded after %ds: %w", cfg.ReviewBaselineTimeoutSec, err))
		}
		return degrade(reason, err)
	}
	if _, err := a.parityGit(ctx, repo, "update-ref", ref, tree, strings.Repeat("0", len(tree))); err != nil {
		return degrade("update_ref", err)
	}
	return nil
}

// logReviewBaseline emits one structured, path-safe diagnostic line. A nil
// request logger keeps tests and embedded use quiet.
func (a *App) logReviewBaseline(repo, reason string, cause error) {
	if a.requestLogger == nil {
		return
	}
	entry := struct {
		Event  string `json:"event"`
		Repo   string `json:"repo"`
		Reason string `json:"reason"`
		Error  string `json:"error,omitempty"`
	}{"amg_review_baseline", repo, reason, ""}
	if cause != nil {
		entry.Error = cause.Error()
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	a.requestLogger.Print(string(encoded))
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
			directories = []string{text("workdir")}
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
