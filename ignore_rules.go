package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

const (
	maxIgnoreFileBytes = 1 << 20
	maxIgnorePatterns  = 4096
)

// IgnoreOptions has CodexFree's enabled-by-default zero value.
type IgnoreOptions struct {
	DisableGitignore       bool
	DisableDefaultPatterns bool
	CustomPatterns         []string
}

var alwaysPrunedNames = map[string]bool{"node_modules": true, ".git": true}

// IgnoreMatcher owns source precedence and directory exclusion. Actual Unicode,
// wildcard, character-class and ** matching belongs to doublestar.
type IgnoreMatcher struct {
	root      string
	opts      IgnoreOptions
	workspace *os.Root // Optional request-owned root, shared by search IO.
	workRoot  string
	mu        sync.Mutex
	rules     map[string][]ignoreRule
}

func NewIgnoreMatcher(workRoot string, opts IgnoreOptions) *IgnoreMatcher {
	return &IgnoreMatcher{root: filepath.Clean(workRoot), workRoot: filepath.Clean(workRoot), opts: opts, rules: map[string][]ignoreRule{}}
}

// IsIgnored never hides the matcher root or paths outside it. Check each parent
// first: Git cannot re-include a child of an excluded directory, even through a
// nested .gitignore. This also preserves the directory-list explicit-root escape.
func (m *IgnoreMatcher) IsIgnored(absPath string, isDir bool) bool {
	if m == nil || m.root == "" {
		return false
	}
	absPath = filepath.Clean(absPath)
	rel, err := filepath.Rel(m.root, absPath)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	ancestor := m.root
	for _, part := range parts[:len(parts)-1] {
		ancestor = filepath.Join(ancestor, part)
		if m.ignoredAt(ancestor, true) {
			return true
		}
	}
	return m.ignoredAt(absPath, isDir)
}

func (m *IgnoreMatcher) ignoredAt(absPath string, isDir bool) bool {
	// Deeper files win; within each source the last matching line wins.
	for dir := filepath.Dir(absPath); ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(dir, absPath)
		if err != nil {
			return false
		}
		rules := m.rulesFor(dir)
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].matches(filepath.ToSlash(rel), isDir) {
				return !rules[i].negated
			}
		}
		if dir == m.root || filepath.Dir(dir) == dir {
			return false
		}
	}
}

func (m *IgnoreMatcher) ShouldPrune(name, absPath string) bool {
	return alwaysPrunedNames[name] || m.IsIgnored(absPath, true)
}

func (m *IgnoreMatcher) rulesFor(dir string) []ignoreRule {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rules, ok := m.rules[dir]; ok {
		return rules
	}
	var rules []ignoreRule
	if dir == m.root {
		if !m.opts.DisableDefaultPatterns {
			for _, name := range sortedDefaultIgnoreNames() {
				rule, _ := parseIgnorePattern(name)
				rules = append(rules, rule)
			}
		}
		if !m.opts.DisableGitignore {
			rules = append(rules, m.readIgnoreFile(filepath.Join(dir, ".git", "info", "exclude"))...)
		}
	}
	if !m.opts.DisableGitignore {
		rules = append(rules, m.readIgnoreFile(filepath.Join(dir, ".gitignore"))...)
	}
	if dir == m.root {
		for _, line := range m.opts.CustomPatterns {
			if rule, ok := parseIgnorePattern(line); ok {
				rules = append(rules, rule)
			}
		}
	}
	m.rules[dir] = rules
	return rules
}

func (m *IgnoreMatcher) readIgnoreFile(absPath string) []ignoreRule {
	var file *os.File
	var err error
	if m.workspace != nil {
		var rel string
		rel, err = workspaceRelativePath(Config{WorkRoot: m.workRoot}, absPath)
		if err == nil {
			file, err = m.workspace.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		}
	} else {
		file, err = openWorkspaceFile(Config{WorkRoot: m.workRoot}, absPath)
	}
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxIgnoreFileBytes+1))
	if err != nil || len(raw) > maxIgnoreFileBytes {
		return nil
	}
	var rules []ignoreRule
	for _, line := range strings.Split(string(raw), "\n") {
		if len(rules) >= maxIgnorePatterns {
			break
		}
		if rule, ok := parseIgnorePattern(line); ok {
			rules = append(rules, rule)
		}
	}
	return rules
}

func sortedDefaultIgnoreNames() []string {
	names := make([]string, 0, len(defaultIgnoredNames))
	for name := range defaultIgnoredNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

type ignoreRule struct {
	pattern  globPattern
	negated  bool
	dirOnly  bool
	anchored bool
}

func (r ignoreRule) matches(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if !r.anchored {
		rel = filepath.Base(rel)
	}
	return globMatches(r.pattern, rel)
}

// Only gitignore line directives live here. The glob engine preserves UTF-8;
// no local wildcard compiler or Cartesian brace expansion is retained.
func parseIgnorePattern(line string) (ignoreRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	for strings.HasSuffix(line, " ") {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			break
		}
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") || len(line) > maxGlobPatternBytes {
		return ignoreRule{}, false
	}
	rule := ignoreRule{negated: strings.HasPrefix(line, "!")}
	if rule.negated {
		line = line[1:]
	}
	rule.dirOnly = strings.HasSuffix(line, "/")
	line = strings.TrimSuffix(line, "/")
	rule.anchored = strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	// Git's trailing /** matches contents, not the directory itself.
	// doublestar also matches the directory, so require one final component.
	if strings.HasSuffix(line, "/**") {
		line += "/*"
	}
	// Braces are literal in gitignore, whereas doublestar supports alternates.
	// Preserve existing escapes and escape only the library's extra syntax.
	var pattern strings.Builder
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if ch == '\\' && i+1 < len(line) {
			pattern.WriteByte(ch)
			i++
			pattern.WriteByte(line[i])
			continue
		}
		if ch == '{' || ch == '}' {
			pattern.WriteByte('\\')
		}
		pattern.WriteByte(ch)
	}
	compiled, err := compileGlobPattern(pattern.String())
	rule.pattern = compiled
	return rule, err == nil
}
