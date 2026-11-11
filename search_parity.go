package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Search inputs intentionally retain CodexFree's names and optional defaults.
type GlobInput struct {
	Path    string `json:"path,omitempty"`
	Pattern string `json:"pattern"`
}
type GrepInput struct {
	Path       string `json:"path,omitempty"`
	Pattern    string `json:"pattern"`
	Context    int    `json:"context,omitempty"`
	FilesOnly  bool   `json:"filesOnly,omitempty"`
	IgnoreCase bool   `json:"ignoreCase,omitempty"`
	Include    string `json:"include,omitempty"`
	MaxResults int    `json:"maxResults,omitempty"`
}

// CodexFree v1.9.1 src/ignore_rules.rs default names. Share the same
// lightweight defaults across tree, list_directory, glob and grep.
var defaultIgnoredNames = map[string]bool{
	"node_modules": true, ".git": true, "dist": true, "build": true,
	"out": true, ".next": true, ".nuxt": true, ".svelte-kit": true,
	".turbo": true, "coverage": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true,
}

// binaryExtensions are never searched by grep, mirroring CodexFree v1.9.1
// src/tools/grep.rs.
var binaryExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".ico": true, ".svg": true, ".woff": true, ".woff2": true, ".ttf": true,
	".eot": true, ".zip": true, ".tar": true, ".gz": true, ".br": true,
	".7z": true, ".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".pdf": true, ".doc": true, ".docx": true, ".mp3": true, ".mp4": true,
	".avi": true, ".mov": true, ".wasm": true, ".o": true, ".a": true,
	".lib": true,
}

// Bound both validation and doublestar's recursive alternate matching before
// filesystem traversal. The branch count is computed without expanding strings.
const (
	maxGlobPatternBytes = 4096
	maxGlobBraceDepth   = 16
	maxGlobBranches     = 256
)

type globPattern string

func compileGlobPattern(pattern string) (globPattern, error) {
	if pattern == "" || len(pattern) > maxGlobPatternBytes {
		return "", errors.New("glob pattern is empty or exceeds 4096 bytes")
	}
	type budget struct{ sum, product int }
	stack := []budget{{product: 1}}
	inClass := false
	for i := 0; i < len(pattern); i++ {
		ch := pattern[i]
		if ch == '\\' {
			i++ // Budget syntax is ASCII; preserve escaped UTF-8 bytes verbatim.
			continue
		}
		if inClass {
			if ch == ']' {
				inClass = false
			}
			continue
		}
		frame := &stack[len(stack)-1]
		switch ch {
		case '[':
			inClass = true
		case '{':
			if len(stack) > maxGlobBraceDepth {
				return "", errors.New("glob alternate nesting exceeds budget")
			}
			stack = append(stack, budget{product: 1})
		case ',':
			if len(stack) > 1 {
				frame.sum += frame.product
				frame.product = 1
			}
		case '}':
			if len(stack) == 1 {
				return "", doublestar.ErrBadPattern
			}
			branches := frame.sum + frame.product
			stack = stack[:len(stack)-1]
			parent := &stack[len(stack)-1]
			if branches > maxGlobBranches || parent.product > maxGlobBranches/branches {
				return "", errors.New("glob alternate branches exceed budget")
			}
			parent.product *= branches
		}
		frame = &stack[len(stack)-1]
		if frame.sum+frame.product > maxGlobBranches {
			return "", errors.New("glob alternate branches exceed budget")
		}
	}
	if len(stack) != 1 || !doublestar.ValidatePattern(pattern) {
		return "", doublestar.ErrBadPattern
	}
	return globPattern(pattern), nil
}

func globMatches(pattern globPattern, path string) bool {
	return doublestar.MatchUnvalidated(string(pattern), path)
}

// patternAllowsDot mirrors the upstream fast-glob dot:false rule: a wildcard
// segment never matches a leading dot, so a pattern only reaches hidden
// files and directories when it names them with a literal dot.
func patternAllowsDot(pattern string) bool {
	for _, segment := range strings.Split(pattern, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	return false
}

// dotOK applies that rule to a candidate relative path.
func dotOK(rel string, allowDot bool) bool {
	if allowDot {
		return true
	}
	for _, segment := range strings.Split(rel, "/") {
		if strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}

// searchWalk walks through the configured root's filesystem. The same Root
// owns enumeration and grep opens, so selected-root symlinks and replacements
// cannot escape WorkRoot between discovery and reading.
func (a *App) searchWalk(ctx context.Context, rootInput string, callback func(*os.Root, string, string, os.DirEntry) error) error {
	c := a.config.Get()
	root, e := resolveConfigPath(c, rootInput)
	if e != nil {
		return e
	}
	workspace, selected, e := openWorkspaceRoot(c, root)
	if e != nil {
		return e
	}
	defer workspace.Close()
	fi, e := workspace.Stat(selected)
	if e != nil {
		return e
	}
	if !fi.IsDir() {
		return errors.New("search path is not a directory")
	}
	matcher := NewIgnoreMatcher(projectIgnoreRoot(c.WorkRoot, root), c.ignoreOptions())
	matcher.workspace, matcher.workRoot = workspace, c.WorkRoot
	visited := 0
	selected = filepath.ToSlash(selected)
	return fs.WalkDir(workspace.FS(), selected, func(name string, ent os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if name == selected {
				return err
			}
			return nil // Unreadable descendants do not abort the entire search.
		}
		visited++
		if visited > 50000 {
			return io.EOF
		}
		absPath := filepath.Join(c.WorkRoot, filepath.FromSlash(name))
		if name != selected {
			if ent.IsDir() {
				if matcher.ShouldPrune(ent.Name(), absPath) {
					return fs.SkipDir
				}
				local, err := filepath.Rel(root, absPath)
				if err != nil {
					return err
				}
				if strings.Count(filepath.ToSlash(local), "/") >= 14 {
					return fs.SkipDir
				}
				return nil
			}
			if matcher.IsIgnored(absPath, false) {
				return nil
			}
		}
		return callback(workspace, absPath, name, ent)
	})
}

func (a *App) globFiles(ctx context.Context, in GlobInput) (TextOut, error) {
	if in.Pattern == "" {
		return TextOut{}, errors.New("glob pattern is required")
	}
	patterns, e := compileGlobPattern(in.Pattern)
	if e != nil {
		// CodexFree treats an unparseable pattern as "no match" instead of
		// failing the call.
		return TextOut{Output: "No files found matching pattern."}, nil
	}
	c := a.config.Get()
	// A basename-only glob is scoped to the selected directory, as in Rust
	// CodexFree; **/*.go discovers nested files.
	base := in.Path
	root, e := resolveConfigPath(c, base)
	if e != nil {
		return TextOut{}, e
	}
	allowDot := patternAllowsDot(in.Pattern)
	maxBytes := c.MaxOutputBytes
	var paths []string
	limitReached := false
	err := a.searchWalk(ctx, base, func(_ *os.Root, path, rel string, ent os.DirEntry) error {
		local, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		local = filepath.ToSlash(local)
		if !globMatches(patterns, local) {
			return nil
		}
		if !dotOK(local, allowDot) {
			return nil
		}
		// CodexFree returns results relative to the selected search root,
		// not the server's unrelated global work_root (/ on this VPS).
		paths = append(paths, local)
		if len(paths) >= 2000 {
			return io.EOF
		}
		return nil
	})
	if err == io.EOF {
		limitReached = true
	} else if err != nil {
		return TextOut{}, err
	}
	sort.Strings(paths)
	if len(paths) == 0 && !limitReached {
		return TextOut{Output: "No files found matching pattern."}, nil
	}
	var b strings.Builder
	for _, p := range paths {
		if b.Len()+len(p)+30 > maxBytes {
			limitReached = true
			break
		}
		fmt.Fprintln(&b, p)
	}
	if limitReached && b.Len()+28 <= maxBytes {
		b.WriteString("[search limit reached]\n")
	}
	return TextOut{Output: b.String()}, nil
}

func (a *App) grepFiles(ctx context.Context, in GrepInput) (TextOut, error) {
	if in.Pattern == "" {
		return TextOut{}, errors.New("pattern is required")
	}
	if in.Context < 0 {
		return TextOut{}, errors.New("context must be >=0")
	}
	if in.Context > 20 {
		in.Context = 20
	}
	if in.MaxResults <= 0 {
		in.MaxResults = 500
	}
	if in.MaxResults > 500 {
		in.MaxResults = 500
	}
	pattern := in.Pattern
	if in.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, e := regexp.Compile(pattern)
	if e != nil {
		return TextOut{}, e
	}
	var include globPattern
	if in.Include != "" {
		include, e = compileGlobPattern(in.Include)
		if e != nil {
			return TextOut{}, fmt.Errorf("invalid include glob %q: %w", in.Include, e)
		}
	}
	maxBytes := a.config.Get().MaxOutputBytes
	var b strings.Builder
	matches, limitReached := 0, false
	err := a.searchWalk(ctx, in.Path, func(workspace *os.Root, path, rel string, ent os.DirEntry) error {
		if matches >= in.MaxResults {
			return io.EOF
		}
		if binaryExtensions[filepath.Ext(ent.Name())] {
			return nil
		}
		if include != "" {
			base := ent.Name()
			if !globMatches(include, rel) && !globMatches(include, base) {
				return nil
			}
		}
		if !ent.Type().IsRegular() {
			return nil
		}
		expected, e := ent.Info()
		if e != nil || !expected.Mode().IsRegular() {
			return nil
		}
		f, info, e := openRootRegularFile(workspace, filepath.FromSlash(rel), expected)
		if e != nil {
			return nil
		}
		if info.Size() > 4*1024*1024 {
			f.Close()
			return nil
		}
		// Keep bounded memory. This also avoids scanner line-length errors on
		// minified assets and makes line context deterministic.
		raw, e := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
		f.Close()
		if e != nil || strings.ContainsRune(string(raw), '\x00') {
			return nil
		}
		lines := strings.Split(string(raw), "\n")
		if in.FilesOnly {
			for _, line := range lines {
				if re.MatchString(line) {
					if b.Len()+len(rel)+20 > maxBytes {
						return io.EOF
					}
					fmt.Fprintln(&b, rel)
					matches++
					break
				}
			}
			return nil
		}
		lastPrinted := -1
		for i, line := range lines {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !re.MatchString(line) {
				continue
			}
			from := i - in.Context
			if from < 0 {
				from = 0
			}
			to := i + in.Context + 1
			if to > len(lines) {
				to = len(lines)
			}
			for j := from; j < to; j++ {
				if j <= lastPrinted {
					continue
				}
				text := lines[j]
				if len(text) > 700 {
					text = text[:700] + "…"
				}
				sep := ":"
				if j != i {
					sep = "-"
				}
				next := fmt.Sprintf("%s%s%d%s%s\n", rel, sep, j+1, sep, text)
				if b.Len()+len(next) > maxBytes-30 {
					return io.EOF
				}
				b.WriteString(next)
				lastPrinted = j
			}
			matches++
			if matches >= in.MaxResults {
				return io.EOF
			}
		}
		return nil
	})
	if err == io.EOF {
		limitReached = true
	} else if err != nil {
		return TextOut{}, err
	}
	if matches == 0 && b.Len() == 0 {
		return TextOut{Output: "No matches found."}, nil
	}
	if limitReached && b.Len()+30 <= maxBytes {
		b.WriteString("[search limit reached]\n")
	}
	return TextOut{Output: b.String()}, nil
}

func (a *App) registerSearchParity(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "glob", Description: "Find project files matching CodexFree glob patterns, supporting ** directory segments, character classes and {a,b} alternates. Optional path scopes the search."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GlobInput) (*mcp.CallToolResult, TextOut, error) {
			out, e := a.globFiles(ctx, in)
			return nil, out, e
		})
	mcp.AddTool(s, &mcp.Tool{Name: "grep", Description: "Regex search project files; supports optional include glob, case folding, filesOnly, context and bounded maxResults."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in GrepInput) (*mcp.CallToolResult, TextOut, error) {
			out, e := a.grepFiles(ctx, in)
			return nil, out, e
		})
}
