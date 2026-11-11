package main

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const reviewUIURI = "ui://agentmcp/review/mcp-app.html"
const reviewMIME = "text/html;profile=mcp-app"
const reviewExtension = "io.modelcontextprotocol/ui"
const reviewMetaKey = "io.github.hypnguyen1209/review"

//go:embed assets/codexfree-review.html
var reviewHTML string

type reviewSummary struct {
	Files       int    `json:"files"`
	Additions   uint64 `json:"additions"`
	Deletions   uint64 `json:"deletions"`
	BinaryFiles int    `json:"binaryFiles"`
}

type reviewFile struct {
	Path         string  `json:"path"`
	PreviousPath string  `json:"previousPath,omitempty"`
	Status       string  `json:"status"`
	Additions    *uint64 `json:"additions,omitempty"`
	Deletions    *uint64 `json:"deletions,omitempty"`
	Binary       bool    `json:"binary"`
}

type reviewPayload struct {
	Summary            reviewSummary `json:"summary"`
	Files              []reviewFile  `json:"files"`
	FilesOmitted       int           `json:"filesOmitted"`
	Patch              string        `json:"patch"`
	PatchIncluded      bool          `json:"patchIncluded"`
	PatchOmittedReason string        `json:"patchOmittedReason,omitempty"`
}

func (a *App) registerReviewUI(s *mcp.Server) {
	meta := mcp.Meta{"ui": map[string]any{"prefersBorder": false}}
	s.AddResource(&mcp.Resource{URI: reviewUIURI, Name: "review", Title: "Code review", MIMEType: reviewMIME, Meta: meta},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: reviewUIURI, MIMEType: reviewMIME, Text: reviewHTML, Meta: meta}}}, nil
		})
}

func (a *App) reviewSnapshot(ctx context.Context, repo, head, pathspec string, env []string) (string, error) {
	if head == "" {
		head = "--empty"
	}
	if _, err := a.gitWithEnv(ctx, repo, env, "read-tree", head); err != nil {
		return "", err
	}
	if _, err := a.gitWithEnv(ctx, repo, env, "--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "add", "-A", "--", pathspec); err != nil {
		return "", err
	}
	tree, err := a.gitWithEnv(ctx, repo, env, "write-tree")
	return strings.TrimSpace(tree.Output), err
}

func (a *App) reviewChanges(ctx context.Context, req *mcp.CallToolRequest, in ShowChangesInput) (ShowChangesOutput, reviewPayload, error) {
	key, err := a.reviewLockKey(ctx, in.Directory)
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	release, err := a.projectGuards.acquire(ctx, []string{key})
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	defer release()

	since := in.Since
	if since == "" {
		since = "last_review"
	}
	if since != "project_open" && since != "last_review" {
		return ShowChangesOutput{}, reviewPayload{}, errors.New("since must be project_open or last_review")
	}
	advance := chatGPTConversationID(req) != ""
	if in.Advance != nil {
		advance = *in.Advance
	}
	state, err := a.existingScopeStatePath(req, in.Scope, "last-review.json")
	if err != nil && (advance || !errors.Is(err, errTaskScopeRequired)) {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	dir, err := resolveConfigPath(a.config.Get(), in.Directory)
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	top, err := a.parityGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, errors.New("select a Git project directory for review")
	}
	repo := strings.TrimSpace(top.Output)
	prefix, err := a.parityGit(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	pathspec := strings.TrimSuffix(strings.TrimSuffix(prefix.Output, "\n"), "/")
	if pathspec == "" {
		pathspec = "."
	}
	headOut, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "HEAD^{tree}")
	unborn := err != nil
	if unborn {
		headOut, err = a.parityGit(ctx, repo, "hash-object", "-t", "tree", "--stdin")
		if err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
	}
	head := strings.TrimSpace(headOut.Output)
	ref := reviewRef(state, repo, pathspec)
	projectOpen := head
	hasProjectOpen := false
	if state != "" {
		if open, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"project-open^{tree}"); err == nil {
			projectOpen, hasProjectOpen = strings.TrimSpace(open.Output), true
		}
	}
	if state != "" && !hasProjectOpen && pathspec != "." {
		if open, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "--quiet", reviewRef(state, repo, ".")+"project-open^{tree}"); err == nil {
			projectOpen, hasProjectOpen = strings.TrimSpace(open.Output), true
		}
	}
	baseline := projectOpen
	if since == "last_review" && state != "" {
		last, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"last-review^{tree}")
		if err == nil {
			baseline = strings.TrimSpace(last.Output)
		} else if legacy := a.legacyReviewTree(ctx, repo, state); legacy != "" {
			baseline = legacy
		}
	}
	tmp, err := os.MkdirTemp("", "amg-review-index-")
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}
	if !advance {
		objects, err := a.parityGit(ctx, repo, "rev-parse", "--path-format=absolute", "--git-path", "objects")
		if err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
		privateObjects := filepath.Join(tmp, "objects")
		if err := os.Mkdir(privateObjects, 0700); err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
		env = append(env, "GIT_OBJECT_DIRECTORY="+privateObjects, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+strconv.Quote(strings.TrimSuffix(objects.Output, "\n")))
	}
	if unborn {
		if _, err := a.gitWithEnv(ctx, repo, env, "hash-object", "-w", "-t", "tree", "--stdin"); err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
	}
	current, err := a.reviewSnapshot(ctx, repo, head, pathspec, env)
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	diffArgs := []string{"--literal-pathspecs", "diff", "--no-ext-diff", "--no-textconv", "--relative", "-M"}
	if pathspec != "." {
		diffArgs[4] = "--relative=" + pathspec
	}
	stats, err := a.gitWithEnv(ctx, repo, env, append(append([]string{}, diffArgs...), "--numstat", "-z", baseline, current, "--", pathspec)...)
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	payload, err := parseReviewStats(stats.Output)
	if err != nil {
		return ShowChangesOutput{}, reviewPayload{}, err
	}
	if len(payload.Files) != 0 {
		names, err := a.gitWithEnv(ctx, repo, env, append(append([]string{}, diffArgs...), "--name-status", "-z", baseline, current, "--", pathspec)...)
		if err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
		applyReviewStatuses(payload.Files, names.Output)
	}
	summary := fmt.Sprintf("%d files changed (+%d -%d, %d binary) since %s.\n", payload.Summary.Files, payload.Summary.Additions, payload.Summary.Deletions, payload.Summary.BinaryFiles, since)
	out := ShowChangesOutput{Output: summary}
	if in.IncludePatch == nil || *in.IncludePatch {
		diff, err := a.gitWithEnv(ctx, repo, env, append(append([]string{}, diffArgs...), "--binary", baseline, current, "--", pathspec)...)
		if err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
		if strings.HasSuffix(diff.Output, "\n[truncated]") {
			payload.PatchOmittedReason = "Patch exceeded the output limit."
			out.Output += payload.PatchOmittedReason
			out.Truncated = true
		} else {
			payload.Patch, payload.PatchIncluded = diff.Output, diff.Output != ""
			out.Output += diff.Output
		}
	}
	if advance {
		if !hasProjectOpen {
			if _, err := a.parityGit(ctx, repo, "update-ref", ref+"project-open", projectOpen); err != nil {
				return ShowChangesOutput{}, reviewPayload{}, err
			}
		}
		if _, err := a.parityGit(ctx, repo, "update-ref", ref+"last-review", current); err != nil {
			return ShowChangesOutput{}, reviewPayload{}, err
		}
	}
	return out, payload, nil
}

func (a *App) legacyReviewTree(ctx context.Context, repo, state string) string {
	raw, err := os.ReadFile(state)
	if err != nil {
		return ""
	}
	var legacy struct{ Repo, Head string }
	if json.Unmarshal(raw, &legacy) != nil || legacy.Repo != repo {
		return ""
	}
	digest, err := hex.DecodeString(legacy.Head)
	if err != nil || (len(digest) != 20 && len(digest) != 32) {
		return ""
	}
	tree, err := a.parityGit(ctx, repo, "rev-parse", "--verify", "--quiet", legacy.Head+"^{tree}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(tree.Output)
}

func parseReviewStats(stats string) (reviewPayload, error) {
	payload := reviewPayload{Files: []reviewFile{}}
	if stats != "" && !strings.HasSuffix(stats, "\x00") {
		return payload, errors.New("review file statistics exceeded the output limit")
	}
	records := strings.Split(strings.TrimSuffix(stats, "\x00"), "\x00")
	for i := 0; i < len(records) && stats != ""; i++ {
		fields := strings.SplitN(records[i], "\t", 3)
		if len(fields) != 3 {
			return payload, errors.New("invalid Git review statistics")
		}
		file := reviewFile{Path: fields[2], Status: "modified"}
		if file.Path == "" {
			if i+2 >= len(records) {
				return payload, errors.New("invalid Git review rename")
			}
			file.PreviousPath, file.Path, file.Status = records[i+1], records[i+2], "renamed"
			i += 2
		}
		if fields[0] == "-" && fields[1] == "-" {
			file.Binary = true
			payload.Summary.BinaryFiles++
		} else {
			add, errAdd := strconv.ParseUint(fields[0], 10, 64)
			del, errDel := strconv.ParseUint(fields[1], 10, 64)
			if errAdd != nil || errDel != nil {
				return payload, errors.New("invalid Git review line counts")
			}
			file.Additions, file.Deletions = &add, &del
			payload.Summary.Additions += add
			payload.Summary.Deletions += del
		}
		payload.Summary.Files++
		if len(payload.Files) < 200 {
			payload.Files = append(payload.Files, file)
		} else {
			payload.FilesOmitted++
		}
	}
	return payload, nil
}

func applyReviewStatuses(files []reviewFile, names string) {
	parts := strings.Split(names, "\x00")
	for i, j := 0, 0; i+1 < len(parts) && j < len(files); j++ {
		status, path := parts[i], parts[i+1]
		if status == "" {
			return
		}
		i += 2
		if status[0] == 'R' || status[0] == 'C' {
			if i >= len(parts) {
				return
			}
			path = parts[i]
			i++
		}
		if path != files[j].Path {
			return
		}
		if label := map[byte]string{'A': "added", 'D': "deleted", 'T': "type_changed", 'C': "copied", 'R': "renamed"}[status[0]]; label != "" {
			files[j].Status = label
		}
	}
}
