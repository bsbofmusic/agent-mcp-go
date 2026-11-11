package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type RunCommandInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Workdir string   `json:"workdir,omitempty" jsonschema:"Optional working directory; defaults to default_workdir."`
}
type TreeInput struct {
	Path  string `json:"path,omitempty"`
	Depth int    `json:"depth,omitempty"`
}
type PlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}
type PlanInput struct {
	Explanation string     `json:"explanation,omitempty"`
	Plan        []PlanStep `json:"plan"`
	Scope       string     `json:"scope,omitempty" jsonschema:"Optional task identity stable across reconnects; otherwise use the current MCP session."`
}
type SleepInput struct {
	DurationMS int `json:"duration_ms"`
}
type GitCommitInput struct {
	Scope     string `json:"scope,omitempty"`
	Message   string `json:"message"`
	All       bool   `json:"all,omitempty"`
	Directory string `json:"directory,omitempty"`
}
type GitPushInput struct {
	Branch    string `json:"branch,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Directory string `json:"directory,omitempty"`
}
type ShowChangesInput struct {
	Directory    string `json:"directory,omitempty"`
	Since        string `json:"since,omitempty"`
	Advance      *bool  `json:"advance,omitempty" jsonschema:"Advance the last-review checkpoint. Defaults to true in ChatGPT and false for generic clients."`
	IncludePatch *bool  `json:"include_patch,omitempty" jsonschema:"Include the patch in the review (default true)."`
	Scope        string `json:"scope,omitempty"`
}
type ShowChangesOutput struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}
type ProjectDocFile struct {
	Path      string `json:"path"`
	Truncated bool   `json:"truncated"`
}
type ProjectDocOut struct {
	Content string           `json:"content"`
	Files   []ProjectDocFile `json:"files"`
}
type ProjectDocInput struct {
	Directory string `json:"directory,omitempty" jsonschema:"Project directory whose AGENTS.md rules you need; absolute, or relative to the configured default working directory. Defaults to the default working directory."`
}
type TimeOut struct {
	CurrentTime string `json:"current_time"`
}

func (a *App) parityRunCommand(ctx context.Context, in RunCommandInput) (TextOut, bool, error) {
	if in.Command == "" || strings.ContainsAny(in.Command, "/\\ ") {
		return TextOut{}, false, errors.New("binary must be a single allowed name")
	}
	if !runBinaryAllowed(in.Command) {
		return TextOut{}, false, fmt.Errorf("binary %s is not in the run_command allowlist", in.Command)
	}
	timeout := time.Duration(in.Timeout) * time.Millisecond
	if in.Timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > time.Duration(a.config.Get().CommandTimeoutSec)*time.Second {
		return TextOut{}, false, errors.New("timeout exceeds config maximum")
	}
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	binary, err := executionBinary(a.config.Get(), in.Command)
	if err != nil {
		return TextOut{}, false, err
	}
	workdir, err := resolveConfigPath(a.config.Get(), in.Workdir)
	if err != nil {
		return TextOut{}, false, err
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return TextOut{}, false, err
	}
	if !info.IsDir() {
		return TextOut{}, false, fmt.Errorf("workdir is not a directory: %s", workdir)
	}
	if err := verifyDirWithinRoot(a.config.Get().WorkRoot, workdir); err != nil {
		return TextOut{}, false, err
	}
	cmd := executionCommand(childCtx, binary, in.Args...)
	cmd.Dir = workdir
	cmd.Env = executionEnvironment(a.config.Get())
	b := &boundedBuffer{limit: a.config.Get().MaxOutputBytes}
	cmd.Stdout = b
	cmd.Stderr = b
	err = runExecution(cmd)
	output, truncated := b.Snapshot()
	if truncated {
		output += "\n[truncated]"
	}
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	output += fmt.Sprintf("\n\nexit code: %d", exitCode)
	if childCtx.Err() != nil {
		output += fmt.Sprintf("\ncontext=%v", childCtx.Err())
	}
	return TextOut{output}, err != nil, nil
}

func (a *App) parityTree(ctx context.Context, in TreeInput) (TextOut, error) {
	c := a.config.Get()
	base := in.Path
	root, err := resolveConfigPath(c, base)
	if err != nil {
		return TextOut{}, err
	}
	depth := in.Depth
	if depth <= 0 {
		depth = 3
	}
	if depth > 10 {
		depth = 10
	}
	nodes := 0
	stopped := false
	var b strings.Builder
	fmt.Fprintln(&b, ".")
	matcher := NewIgnoreMatcher(projectIgnoreRoot(c.WorkRoot, root), c.ignoreOptions())
	var rec func(string, int, string) error
	rec = func(path string, level int, prefix string) error {
		if level >= depth || stopped {
			return nil
		}
		entries, e := readWorkspaceDirectory(c, path)
		if e != nil {
			return e
		}
		var selected []os.DirEntry
		for _, ent := range entries {
			abs := filepath.Join(path, ent.Name())
			ignored := matcher.IsIgnored(abs, ent.IsDir())
			if ent.IsDir() {
				ignored = matcher.ShouldPrune(ent.Name(), abs)
			}
			if !ignored {
				selected = append(selected, ent)
			}
		}
		sort.Slice(selected, func(i, j int) bool {
			if selected[i].IsDir() != selected[j].IsDir() {
				return selected[i].IsDir()
			}
			return selected[i].Name() < selected[j].Name()
		})
		for i, ent := range selected {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if nodes >= 800 || b.Len() >= c.MaxOutputBytes-150 {
				stopped = true
				break
			}
			nodes++
			marker := "├── "
			childPrefix := "│   "
			if i == len(selected)-1 {
				marker = "└── "
				childPrefix = "    "
			}
			name := ent.Name()
			if ent.IsDir() {
				name += "/"
			}
			fmt.Fprintln(&b, prefix+marker+name)
			if ent.IsDir() {
				if err := rec(filepath.Join(path, ent.Name()), level+1, prefix+childPrefix); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err = rec(root, 0, ""); err != nil {
		return TextOut{}, err
	}
	if stopped {
		fmt.Fprintf(&b, "\n(stopped at %d nodes; lower depth or select a subdirectory)\n", nodes)
	}
	return TextOut{strings.TrimSuffix(b.String(), "\n")}, nil
}

func (a *App) parityUpdatePlan(ctx context.Context, req *mcp.CallToolRequest, in PlanInput) (TextOut, error) {
	inProgress := 0
	for _, s := range in.Plan {
		if strings.TrimSpace(s.Step) == "" {
			return TextOut{}, errors.New("blank plan step")
		}
		switch s.Status {
		case "in_progress":
			inProgress++
		case "pending", "completed":
		default:
			return TextOut{}, errors.New("invalid plan status")
		}
	}
	if inProgress > 1 {
		return TextOut{}, errors.New("at most one step may be in_progress")
	}
	if len(in.Plan) > 80 {
		return TextOut{}, errors.New("too many plan steps")
	}
	b, e := json.MarshalIndent(in, "", "  ")
	if e != nil {
		return TextOut{}, e
	}
	if len(b) > 32*1024 {
		return TextOut{}, errors.New("plan is too large")
	}
	// Serialize writes per scope directory only: independent tasks keep
	// running in parallel, and a canceled caller never blocks another agent.
	err := a.withScopeState(ctx, req, in.Scope, true, func(dir string, _ bool) error {
		return atomicFile(filepath.Join(dir, "plan.json"), b, 0600)
	})
	if err != nil {
		return TextOut{}, err
	}
	var out strings.Builder
	for _, s := range in.Plan {
		mark := "[ ]"
		if s.Status == "completed" {
			mark = "[x]"
		} else if s.Status == "in_progress" {
			mark = "[~]"
		}
		fmt.Fprintln(&out, mark, s.Step)
	}
	return TextOut{out.String()}, nil
}

func (a *App) parityDoc() (ProjectDocOut, error) {
	return a.projectDocAt("")
}

func (a *App) projectDocAt(directory string) (ProjectDocOut, error) {
	cfg := a.config.Get()
	root := filepath.Clean(cfg.WorkRoot)
	candidates := []string{filepath.Join(root, "AGENTS.md")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "AGENTS.md"))
	}
	rootFD, err := os.OpenRoot(root)
	if err != nil {
		return ProjectDocOut{}, err
	}
	defer rootFD.Close()
	// Without an explicit directory the project rules start at the configured
	// default working directory, so the AGENTS.md that actually governs the
	// default scope is the first specific candidate.
	target := cfg.defaultWorkdir()
	if strings.TrimSpace(directory) != "" {
		target, err = resolveConfigPath(cfg, directory)
		if err != nil {
			return ProjectDocOut{}, err
		}
	}
	if target != root {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			return ProjectDocOut{}, err
		}
		if info, err := rootFD.Stat(rel); err != nil || !info.IsDir() {
			return ProjectDocOut{}, errors.New("project directory is unavailable or outside the workspace")
		}
		var chain []string
		for dir := target; dir != root && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
			chain = append(chain, filepath.Join(dir, "AGENTS.md"))
		}
		for i := len(chain) - 1; i >= 0; i-- {
			candidates = append(candidates, chain[i])
		}
	}
	seen := map[string]bool{}
	out := ProjectDocOut{Files: []ProjectDocFile{}}
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		rel, err := filepath.Rel(root, p)
		if err != nil || !filepath.IsLocal(rel) {
			continue
		}
		f, e := rootFD.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			continue
		}
		if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
			f.Close()
			continue
		}
		raw, e := io.ReadAll(io.LimitReader(f, 128*1024+1))
		f.Close()
		if e != nil {
			return ProjectDocOut{}, e
		}
		truncated := len(raw) > 128*1024
		if truncated {
			raw = raw[:128*1024]
		}
		if len(out.Content)+len(raw) > cfg.MaxOutputBytes {
			rem := cfg.MaxOutputBytes - len(out.Content)
			if rem <= 0 {
				break
			}
			if rem < len(raw) {
				raw = raw[:rem]
				truncated = true
			}
		}
		out.Content += fmt.Sprintf("\n### %s\n%s\n", p, string(raw))
		out.Files = append(out.Files, ProjectDocFile{Path: p, Truncated: truncated})
	}
	return out, nil
}

func (a *App) parityGit(ctx context.Context, dir string, args ...string) (TextOut, error) {
	return a.gitWithEnv(ctx, dir, nil, args...)
}

func (a *App) gitWithEnv(ctx context.Context, dir string, env []string, args ...string) (TextOut, error) {
	c := a.config.Get()
	p, e := resolveConfigPath(c, dir)
	if e != nil {
		return TextOut{}, e
	}
	if e := verifyDirWithinRoot(c.WorkRoot, p); e != nil {
		return TextOut{}, e
	}
	cc, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	binary, err := executionBinary(c, "git")
	if err != nil {
		return TextOut{}, err
	}
	cmd := executionCommand(cc, binary, append([]string{"-C", p}, args...)...)
	cmd.Env = append(executionEnvironment(c), env...)
	b := &boundedBuffer{limit: c.MaxOutputBytes}
	cmd.Stdout = b
	cmd.Stderr = b
	e = runExecution(cmd)
	str, tr := b.Snapshot()
	if tr {
		str += "\n[truncated]"
	}
	if e != nil {
		return TextOut{str}, fmt.Errorf("git: %w", e)
	}
	return TextOut{str}, nil
}

func (a *App) registerParity(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "run_command", Description: "Execute one allowlisted binary with argv, no shell parsing, bounded timeout/output."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in RunCommandInput) (*mcp.CallToolResult, TextOut, error) {
			v, failed, e := a.parityRunCommand(ctx, in)
			return &mcp.CallToolResult{IsError: failed}, v, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "tree", Description: "Bounded ASCII tree of the selected project directory, skipping generated/vendor dirs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in TreeInput) (*mcp.CallToolResult, TextOut, error) {
			v, e := a.parityTree(ctx, in)
			return nil, v, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "update_plan", Description: "Persist the current task's plan steps and progress on disk across conversation changes."},
		func(ctx context.Context, req *mcp.CallToolRequest, in PlanInput) (*mcp.CallToolResult, TextOut, error) {
			v, e := a.parityUpdatePlan(ctx, req, in)
			return nil, v, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "clock_curr_time", Description: "Get the current UTC time."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, TimeOut, error) {
			return nil, TimeOut{CurrentTime: time.Now().UTC().Format("2006-01-02 15:04:05 UTC")}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "clock_sleep", Description: "Pause the current tool call for 1..300000 milliseconds (cancellable)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SleepInput) (*mcp.CallToolResult, TextOut, error) {
			if in.DurationMS < 1 || in.DurationMS > 300000 {
				return nil, TextOut{}, errors.New("duration_ms must be between 1 and 300000")
			}
			t := time.NewTimer(time.Duration(in.DurationMS) * time.Millisecond)
			defer t.Stop()
			begin := time.Now()
			select {
			case <-ctx.Done():
				return nil, TextOut{}, ctx.Err()
			case <-t.C:
			}
			return nil, TextOut{fmt.Sprintf("slept %d ms", time.Since(begin).Milliseconds())}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_project_doc", Description: "Read AGENTS.md rules for the selected VPS project, from workspace rules to the most specific directory. Call after identifying the project and before changing it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ProjectDocInput) (*mcp.CallToolResult, ProjectDocOut, error) {
			v, e := a.projectDocAt(in.Directory)
			return nil, v, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "get_agent_brief", Description: "Start here before a VPS task when the client did not display the server's setup instructions; call it first whenever the host does not surface MCP initialize instructions. Read environment, canonical Skills, shared tools, AGENTS.md and current task progress. The current ChatGPT conversation is recognized automatically; otherwise pass an existing explicit scope when resuming."},
		func(ctx context.Context, req *mcp.CallToolRequest, in AgentBriefInput) (*mcp.CallToolResult, TextOut, error) {
			// As in CodexFree, initialize gives the generic brief. Task-local
			// state requires the actual scoped tool-call identity or explicit
			// stable scope; never infer another client's scratch history.
			brief := a.buildAgentInstructions()
			handover, err := a.taskHandover(ctx, req, in.Scope)
			if err != nil {
				if !errors.Is(err, errTaskScopeRequired) {
					return nil, TextOut{}, err
				}
				brief += "\n\nNo task scope was resolved; saved plan/notes were not restored. Reuse an explicit task scope to resume."
			} else if handover != "" {
				brief = strings.Replace(brief, "\n## Available Skills",
					"\n## Saved state (current task only)\n\n"+handover+"\n## Available Skills", 1)
			} else {
				brief += "\n\nNo saved plan or notes exist for this task scope. Start this task fresh; do not search or import another task's state."
			}
			return nil, TextOut{brief}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "git_commit", Description: "Commit staged changes in a selected repository; optional -a stages tracked files only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GitCommitInput) (*mcp.CallToolResult, TextOut, error) {
			if strings.TrimSpace(in.Message) == "" {
				return nil, TextOut{}, errors.New("commit message is required")
			}
			args := []string{"commit"}
			if in.All {
				args = append(args, "-a")
			}
			args = append(args, "-m", in.Message)
			out, e := a.parityGit(ctx, in.Directory, args...)
			return nil, out, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "git_push", Description: "Push a selected Git checkout to a remote branch."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GitPushInput) (*mcp.CallToolResult, TextOut, error) {
			remote := in.Remote
			if remote == "" {
				remote = "origin"
			}
			if strings.HasPrefix(remote, "-") {
				return nil, TextOut{}, errors.New("invalid remote")
			}
			args := []string{"push", remote}
			if in.Branch != "" {
				if strings.HasPrefix(in.Branch, "-") {
					return nil, TextOut{}, errors.New("invalid branch")
				}
				args = append(args, in.Branch)
			}
			out, e := a.parityGit(ctx, in.Directory, args...)
			return nil, out, e
		})
	mcp.AddTool(s, &mcp.Tool{
		Name: "show_changes", Title: "Review workspace changes",
		Description: "Review changes in the selected Git directory since the last review or the original baseline. Returns a textual diff and a review card in MCP Apps clients.",
		Meta:        mcp.Meta{"ui": map[string]any{"resourceUri": reviewUIURI}, "openai/outputTemplate": reviewUIURI},
	},
		func(ctx context.Context, req *mcp.CallToolRequest, in ShowChangesInput) (*mcp.CallToolResult, ShowChangesOutput, error) {
			out, payload, err := a.reviewChanges(ctx, req, in)
			if err != nil {
				return nil, out, err
			}
			if req.Session != nil {
				if init := req.Session.InitializeParams(); init != nil && init.Capabilities != nil {
					if _, ok := init.Capabilities.Extensions[reviewExtension]; ok && payload.PatchIncluded {
						out.Output = strings.TrimSuffix(out.Output, payload.Patch)
					}
				}
			}
			return &mcp.CallToolResult{Meta: mcp.Meta{reviewMetaKey: payload}}, out, nil
		})
}
