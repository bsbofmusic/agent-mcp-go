package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TextOut struct {
	Output string `json:"output"`
}
type PathInput struct {
	Path string `json:"path,omitempty" jsonschema:"Optional path; absolute, or relative to the configured default working directory. Defaults to the default working directory."`
}
type ReadInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty" jsonschema:"Zero-based starting line."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of lines; default 200."`
}
type WriteInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Scope   string `json:"scope,omitempty" jsonschema:"Stable task scope for review and reconnection continuity."`
}
type SkillFilter struct {
	Search string `json:"search,omitempty"`
}
type SkillReadInput struct {
	Name     string `json:"name"`
	Resource string `json:"resource,omitempty" jsonschema:"Skill-relative support document; default SKILL.md."`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}
type NoteInput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope,omitempty" jsonschema:"Optional task identifier for durable continuity across reconnects."`
}
type RecallInput struct {
	Scope string `json:"scope,omitempty"`
}
type SessionInput struct {
	SessionID       int64  `json:"session_id"`
	Chars           string `json:"chars,omitempty"`
	YieldTimeMS     int    `json:"yield_time_ms,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
}
type SettingsInput struct {
	MaxFileBytes          int64 `json:"max_file_bytes"`
	MaxOutputBytes        int   `json:"max_output_bytes"`
	CommandTimeoutSeconds int   `json:"command_timeout_seconds"`
}
type GitInput struct {
	Directory string `json:"directory,omitempty" jsonschema:"Existing Git checkout path; absolute, or relative to the configured default working directory. Defaults to the default working directory."`
}
type GitLogInput struct {
	Directory string `json:"directory,omitempty"`
	Count     int    `json:"count,omitempty"`
	Oneline   bool   `json:"oneline,omitempty"`
}

func execSessionOwner(ctx context.Context, req *mcp.CallToolRequest) string {
	if mode, _ := ctx.Value(mcpHTTPModeKey{}).(string); mode == "stateless" {
		// Official Go SDK creates a fresh transport session for every
		// Stateless HTTP POST. Continuity in that mode relies on the
		// unpredictable, 52-bit numeric capability handle.
		return ""
	}
	if req != nil && req.Session != nil {
		return req.Session.ID()
	}
	return ""
}

func resolveWorkPath(root, requested string) (string, error) {
	if requested == "" {
		requested = root
	}
	var p string
	if filepath.IsAbs(requested) {
		p = filepath.Clean(requested)
	} else {
		p = filepath.Join(root, requested)
	}
	r, err := filepath.Rel(filepath.Clean(root), p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", errors.New("path escapes configured work_root")
	}
	return p, nil
}

// resolveConfigPath resolves a caller-supplied path against the configured
// default working directory. Empty paths and relative paths are anchored at
// Config.defaultWorkdir(); absolute paths are honoured as-is. WorkRoot stays
// the security confinement boundary in every case.
func resolveConfigPath(c Config, requested string) (string, error) {
	base := c.defaultWorkdir()
	if strings.TrimSpace(requested) == "" {
		requested = base
	} else if !filepath.IsAbs(requested) {
		requested = filepath.Join(base, requested)
	}
	return resolveWorkPath(c.WorkRoot, requested)
}

// Same 1024-based compact sizes as CodexFree's list_directory.
func codexSize(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	}
	if bytes < 1024*1024 {
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

func (a *App) registerCore(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "exec_command", Description: "Run a POSIX shell command on the VPS as the agent user; nonblocking after yield_time_ms and returns a resumable session ID. If the host did not surface the MCP initialize instructions, call get_agent_brief first."},
		func(ctx context.Context, req *mcp.CallToolRequest, in CommandInput) (*mcp.CallToolResult, CommandOutput, error) {
			if err := ctx.Err(); err != nil {
				return nil, CommandOutput{}, err
			}
			logRequestPhase(ctx, "command_start")
			session, err := a.sessions.startForOwner(a.config.Get(), in, execSessionOwner(ctx, req))
			if err != nil {
				return nil, CommandOutput{}, err
			}
			logRequestPhase(ctx, "command_wait")
			out, err := session.waitContext(ctx, in.YieldTimeMS)
			if err != nil {
				return nil, CommandOutput{}, fmt.Errorf("session_id=%d; command started, output retained; do not replay automatically: %w", session.ID, err)
			}
			return &mcp.CallToolResult{IsError: out.Finished && out.ExitCode != 0}, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "write_stdin", Description: "Send stdin to a running exec_command session and poll bounded output. Poll with empty chars."},
		func(ctx context.Context, req *mcp.CallToolRequest, in SessionInput) (*mcp.CallToolResult, CommandOutput, error) {
			session, err := a.sessions.getForOwner(in.SessionID, execSessionOwner(ctx, req))
			if err != nil {
				return nil, CommandOutput{}, err
			}
			if err := ctx.Err(); err != nil {
				return nil, CommandOutput{}, err
			}
			if in.Chars == "\x03" {
				// CodexFree's write_stdin maps Ctrl-C to process-group
				// cancellation instead of writing ETX to a plain pipe.
				session.stop()
			} else if in.Chars != "" {
				logRequestPhase(ctx, "command_input")
				if _, err = session.writeContext(ctx, in.Chars); err != nil {
					return nil, CommandOutput{}, fmt.Errorf("session_id=%d: %w", session.ID, err)
				}
			}
			logRequestPhase(ctx, "command_wait")
			out, err := session.waitContext(ctx, in.YieldTimeMS)
			if err != nil {
				return nil, CommandOutput{}, fmt.Errorf("session_id=%d; output retained, prior input may have been consumed; do not replay automatically: %w", session.ID, err)
			}
			return &mcp.CallToolResult{IsError: out.Finished && out.ExitCode != 0}, out, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "stop_command", Description: "Stop a running shell session and its process group."},
		func(ctx context.Context, req *mcp.CallToolRequest, in SessionInput) (*mcp.CallToolResult, TextOut, error) {
			session, err := a.sessions.getForOwner(in.SessionID, execSessionOwner(ctx, req))
			if err != nil {
				return nil, TextOut{}, err
			}
			session.stop()
			return nil, TextOut{"termination requested"}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "read_file", Description: "Read a bounded numbered window from a VPS text file."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, TextOut, error) {
			c := a.config.Get()
			path, err := resolveConfigPath(c, in.Path)
			if err != nil {
				return nil, TextOut{}, err
			}
			if in.Offset < 0 {
				return nil, TextOut{}, errors.New("offset must be nonnegative")
			}
			if in.Limit <= 0 {
				in.Limit = 200
			}
			if in.Limit > 1000 {
				in.Limit = 1000
			}
			if err := ctx.Err(); err != nil {
				return nil, TextOut{}, err
			}
			logRequestPhase(ctx, "file_open")
			f, err := openWorkspaceFile(c, path)
			if err != nil {
				return nil, TextOut{}, err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return nil, TextOut{}, err
			}
			if !info.Mode().IsRegular() {
				return nil, TextOut{}, errors.New("read_file requires a regular file")
			}
			logRequestPhase(ctx, "file_read")
			r := bufio.NewScanner(f)
			r.Buffer(make([]byte, 4096), 4*1024*1024)
			var b strings.Builder
			last := in.Offset
			more := false
			for n := 0; ; n++ {
				if err := ctx.Err(); err != nil {
					return nil, TextOut{}, err
				}
				if !r.Scan() {
					break
				}
				if n < in.Offset {
					continue
				}
				if n >= in.Offset+in.Limit {
					more = true
					break
				}
				line := r.Text()
				if b.Len()+len(line)+40 > c.MaxOutputBytes {
					b.WriteString("\n[output capped]")
					break
				}
				fmt.Fprintf(&b, "%d	%s\n", n+1, line)
				last = n + 1
			}
			if err = r.Err(); err != nil {
				return nil, TextOut{}, err
			}
			if more {
				fmt.Fprintf(&b, "\n(showing lines %d-%d; request offset=%d for more)\n", in.Offset+1, last, last)
			}
			return nil, TextOut{b.String()}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "write_file", Description: "Atomically write a text file in the permitted workspace."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in WriteInput) (*mcp.CallToolResult, TextOut, error) {
			c := a.config.Get()
			path, err := resolveConfigPath(c, in.Path)
			if err != nil {
				return nil, TextOut{}, err
			}
			if int64(len(in.Content)) > c.MaxFileBytes {
				return nil, TextOut{}, errors.New("file too large: use transfer_prepare for large binary files")
			}
			if err = atomicWorkspaceFile(c, path, []byte(in.Content)); err != nil {
				return nil, TextOut{}, err
			}
			return nil, TextOut{fmt.Sprintf("written %d bytes to %s", len(in.Content), path)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "list_directory", Description: "List filtered directory entries with CodexFree type, human-readable size and name."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PathInput) (*mcp.CallToolResult, TextOut, error) {
			c := a.config.Get()
			p, e := resolveConfigPath(c, in.Path)
			if e != nil {
				return nil, TextOut{}, e
			}
			entries, e := readWorkspaceDirectory(c, p)
			if e != nil {
				return nil, TextOut{}, e
			}
			matcher := NewIgnoreMatcher(projectIgnoreRoot(c.WorkRoot, p), c.ignoreOptions())
			targetIgnored := matcher.IsIgnored(p, true)
			visible := make([]os.DirEntry, 0, len(entries))
			for _, ent := range entries {
				if targetIgnored || !matcher.IsIgnored(filepath.Join(p, ent.Name()), ent.IsDir()) {
					visible = append(visible, ent)
				}
			}
			if len(visible) == 0 {
				return nil, TextOut{"Directory is empty."}, nil
			}
			var b strings.Builder
			shown := 0
			for _, x := range visible {
				if shown >= 300 {
					break
				}
				line := ""
				if x.IsDir() {
					line = fmt.Sprintf("dir\t-\t%s/\n", x.Name())
				} else {
					fi, err := statWorkspacePath(c, filepath.Join(p, x.Name()))
					if err != nil {
						return nil, TextOut{}, err
					}
					line = fmt.Sprintf("file\t%s\t%s\n", codexSize(fi.Size()), x.Name())
				}
				if b.Len()+len(line)+128 > c.MaxOutputBytes {
					break
				}
				b.WriteString(line)
				shown++
			}
			if shown < len(visible) {
				fmt.Fprintf(&b, "\n(showing %d of %d entries; use glob for a filtered view)", shown, len(visible))
			}
			return nil, TextOut{strings.TrimSuffix(b.String(), "\n")}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "git_status", Description: "Get current git status in a selected checkout."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GitInput) (*mcp.CallToolResult, TextOut, error) {
			out, err := a.gitOp(ctx, in.Directory, "status", "--long")
			return nil, TextOut{out}, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "git_log", Description: "Read latest Git commits; optional count and oneline output, directory defaults to the default working directory."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GitLogInput) (*mcp.CallToolResult, TextOut, error) {
			if in.Count <= 0 {
				in.Count = 10
			}
			if in.Count > 100 {
				in.Count = 100
			}
			args := []string{"log", fmt.Sprintf("-%d", in.Count)}
			if in.Oneline {
				args = append(args, "--oneline")
			}
			out, err := a.gitOp(ctx, in.Directory, args...)
			return nil, TextOut{out}, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "skills_list", Description: "Dynamically list the canonical Skills Manager Skills with name, description, path and scope; optional search filter."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SkillFilter) (*mcp.CallToolResult, SkillListOut, error) {
			v, err := a.listCanonicalSkills(in.Search)
			return nil, v, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "skills_read", Description: "Read a canonical Skill or one of its support files on demand."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SkillReadInput) (*mcp.CallToolResult, SkillReadOut, error) {
			v, err := a.readCanonicalSkill(in)
			return nil, v, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "remember", Description: "Save bounded task-local notes, NOT shared long-term history. Pass an empty value to delete a key. For durable memory, discover its existing module with mcp_list_sources and use mcp_get_tool/mcp_call_tool."},
		func(ctx context.Context, req *mcp.CallToolRequest, in NoteInput) (*mcp.CallToolResult, TextOut, error) {
			v, err := a.rememberNote(ctx, req, in)
			return nil, v, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "recall", Description: "Read task-local saved notes; use shared MCP MemOS route for long-term semantic recall."},
		func(ctx context.Context, req *mcp.CallToolRequest, in RecallInput) (*mcp.CallToolResult, TextOut, error) {
			v, err := a.recallNotes(ctx, req, in)
			return nil, v, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "settings_get", Description: "Read active agent-mcp-go tunable settings (no credentials)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, TextOut, error) {
			c := a.config.Get()
			return nil, TextOut{fmt.Sprintf("max_file_bytes=%d\nmax_output_bytes=%d\ncommand_timeout_seconds=%d", c.MaxFileBytes, c.MaxOutputBytes, c.CommandTimeoutSec)}, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "settings_update", Description: "Hot-update three bounded settings atomically; other security and routing fields remain fixed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in SettingsInput) (*mcp.CallToolResult, TextOut, error) {
			c, err := a.config.Update(in.MaxFileBytes, in.MaxOutputBytes, in.CommandTimeoutSeconds)
			if err != nil {
				return nil, TextOut{}, err
			}
			return nil, TextOut{fmt.Sprintf("saved and active: max_file_bytes=%d max_output_bytes=%d command_timeout_seconds=%d", c.MaxFileBytes, c.MaxOutputBytes, c.CommandTimeoutSec)}, nil
		})
}

type bytesBuffer struct{ b strings.Builder }

// rememberNote stores one bounded task-local note for the caller's scope.
// Writes are serialized per scope only: independent agents keep their own
// scopes parallel, and a canceled caller never blocks another agent.
func (a *App) rememberNote(ctx context.Context, req *mcp.CallToolRequest, in NoteInput) (TextOut, error) {
	if in.Key == "" || len(in.Key) > 120 || len(in.Value) > 2048 {
		return TextOut{}, errors.New("invalid note size")
	}
	var out TextOut
	err := a.withScopeState(ctx, req, in.Scope, true, func(dir string, _ bool) error {
		path := filepath.Join(dir, "notes.json")
		notes := map[string]string{}
		if b, e := os.ReadFile(path); e == nil {
			if e = json.Unmarshal(b, &notes); e != nil {
				return e
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		if notes == nil {
			return errors.New("task notes must be a JSON object")
		}
		if in.Value == "" {
			delete(notes, in.Key)
		} else {
			notes[in.Key] = in.Value
		}
		if len(notes) > 200 {
			return errors.New("too many task notes")
		}
		b, _ := json.MarshalIndent(notes, "", "  ")
		if err := atomicFile(path, b, 0600); err != nil {
			return err
		}
		out = TextOut{"task-local note stored"}
		return nil
	})
	return out, err
}

// recallNotes returns the task-local notes of the caller's scope.
func (a *App) recallNotes(ctx context.Context, req *mcp.CallToolRequest, in RecallInput) (TextOut, error) {
	var out TextOut
	err := a.withScopeState(ctx, req, in.Scope, false, func(dir string, _ bool) error {
		b, err := os.ReadFile(filepath.Join(dir, "notes.json"))
		if os.IsNotExist(err) {
			out = TextOut{"{}"}
			return nil
		}
		if err != nil {
			return err
		}
		out = TextOut{string(b)}
		return nil
	})
	return out, err
}

func (b *bytesBuffer) Write(p []byte) (int, error) {
	if b.b.Len() < 256*1024 {
		remain := 256*1024 - b.b.Len()
		if len(p) > remain {
			b.b.Write(p[:remain])
		} else {
			b.b.Write(p)
		}
	}
	return len(p), nil
}
func (b *bytesBuffer) String() string { return b.b.String() }

func (a *App) gitOp(ctx context.Context, dir string, args ...string) (string, error) {
	c := a.config.Get()
	p, err := resolveConfigPath(c, dir)
	if err != nil {
		return "", err
	}
	if err := verifyDirWithinRoot(c.WorkRoot, p); err != nil {
		return "", err
	}
	cc, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append([]string{"-C", p}, args...)
	binary, err := executionBinary(c, "git")
	if err != nil {
		return "", err
	}
	cmd := executionCommand(cc, binary, argv...)
	cmd.Env = executionEnvironment(c)
	var b bytesBuffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err = runExecution(cmd)
	return b.String(), err
}
