package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
	Scope       string `json:"scope"`
}
type SkillListOut struct {
	Skills  []SkillInfo `json:"skills"`
	Content string      `json:"content"`
}
type SkillReadOut struct {
	Content string `json:"content"`
}

const (
	// skillFrontmatterBytes bounds how much of a SKILL.md is read, so an
	// oversized file can never force an unbounded allocation.
	skillFrontmatterBytes = 64 * 1024
	skillDescriptionBytes = 700
	skillDescriptionTail  = "…"
)

// readSkillFrontmatter returns the YAML block between the opening and closing
// frontmatter fences, or false when the file does not start with "---" or the
// block is never closed. YAML parsing itself is delegated to yaml.v3 instead of
// hand-rolled string slicing, which broke on quoted colons, escaped quotes and
// block scalars longer than eight lines.
func readSkillFrontmatter(r io.Reader, limit int64) (string, bool) {
	br := bufio.NewReader(io.LimitReader(r, limit))
	first, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	if strings.TrimRight(first, "\r\n") != "---" {
		return "", false
	}
	var body strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "---" || trimmed == "..." {
			return body.String(), true
		}
		body.WriteString(trimmed)
		body.WriteByte('\n')
		if err != nil {
			// EOF or the 64 KiB budget ran out before a closing fence.
			return "", false
		}
	}
}

// truncateUTF8 cuts s to at most limit bytes on a UTF-8 rune boundary. The
// reported bool is true when bytes were dropped.
func truncateUTF8(s string, limit int) (string, bool) {
	if limit >= len(s) {
		return s, false
	}
	if limit < 0 {
		limit = 0
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// openSkillRegularFile opens relPath inside skillBase using os.Root so symlink
// swaps cannot escape skillBase (TOCTOU) and O_NONBLOCK + IsRegular prevent
// blocking on FIFOs or device nodes.
func openSkillRegularFile(skillBase, relPath string) (*os.File, error) {
	if !filepath.IsLocal(relPath) {
		return nil, errors.New("resource escapes skill directory")
	}
	root, err := os.OpenRoot(skillBase)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(relPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("skill resource must be a regular file")
	}
	return f, nil
}

func skillDescriptionFromBase(skillBase string) (string, bool) {
	f, err := openSkillRegularFile(skillBase, "SKILL.md")
	if err != nil {
		return "", false
	}
	defer f.Close()
	block, ok := readSkillFrontmatter(f, skillFrontmatterBytes)
	if !ok {
		return "", true
	}
	var fm struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return "", true
	}
	value := strings.Join(strings.Fields(fm.Description), " ")
	if clipped, cut := truncateUTF8(value, skillDescriptionBytes); cut {
		value = strings.TrimRight(clipped, " ") + skillDescriptionTail
	}
	return value, true
}

func skillDescription(path string) string {
	f, err := openSkillRegularFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	block, ok := readSkillFrontmatter(f, skillFrontmatterBytes)
	if !ok {
		return ""
	}
	var fm struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return ""
	}
	value := strings.Join(strings.Fields(fm.Description), " ")
	if clipped, cut := truncateUTF8(value, skillDescriptionBytes); cut {
		value = strings.TrimRight(clipped, " ") + skillDescriptionTail
	}
	return value
}

func (a *App) listCanonicalSkills(filter string) (SkillListOut, error) {
	c := a.config.Get()
	entries, err := os.ReadDir(c.SkillsRoot)
	if err != nil {
		return SkillListOut{}, err
	}
	skills := []SkillInfo{}
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(ent.Name()), strings.ToLower(filter)) {
			continue
		}
		skillBase := filepath.Join(c.SkillsRoot, ent.Name())
		desc, ok := skillDescriptionFromBase(skillBase)
		if !ok {
			continue
		}
		path := filepath.Join(skillBase, "SKILL.md")
		skills = append(skills, SkillInfo{
			Name: ent.Name(), Description: desc,
			Path: path, Scope: "user",
		})
	}
	var b strings.Builder
	for _, skill := range skills {
		fmt.Fprintf(&b, "%s — %s\n", skill.Name, skill.Description)
		if b.Len() > c.MaxOutputBytes-256 {
			break
		}
	}
	content := b.String()
	if clipped, cut := truncateUTF8(content, c.MaxOutputBytes); cut {
		// Step back to a rune boundary so Chinese skill lists never emit
		// invalid UTF-8 when the byte cap lands mid-character.
		content = clipped + "\n[truncated]"
	}
	return SkillListOut{Skills: skills, Content: content}, nil
}

func (a *App) readCanonicalSkill(in SkillReadInput) (SkillReadOut, error) {
	c := a.config.Get()
	if in.Name == "" || in.Name == "." || !filepath.IsLocal(in.Name) || in.Name != filepath.Base(in.Name) || strings.Contains(in.Name, "..") {
		return SkillReadOut{}, errors.New("invalid skill name")
	}
	base := filepath.Clean(filepath.Join(c.SkillsRoot, in.Name))
	resource := in.Resource
	if resource == "" {
		resource = "SKILL.md"
	}
	if filepath.IsAbs(resource) {
		return SkillReadOut{}, errors.New("resource must be relative")
	}
	rel := filepath.Clean(resource)
	if !filepath.IsLocal(rel) {
		return SkillReadOut{}, errors.New("resource escapes skill directory")
	}
	if in.Offset < 0 {
		return SkillReadOut{}, errors.New("offset must be nonnegative")
	}
	if in.Limit <= 0 {
		in.Limit = 230
	}
	if in.Limit > 1000 {
		in.Limit = 1000
	}
	f, err := openSkillRegularFile(base, rel)
	if err != nil {
		return SkillReadOut{}, err
	}
	defer f.Close()
	displayPath := filepath.Join(base, rel)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var b strings.Builder
	// CodexFree skills_read preserves the original Markdown body rather than
	// wrapping every line in a numbered file-view. A numbered YAML frontmatter
	// is not valid YAML and wastes context in newly attached agents.
	fmt.Fprintf(&b, "%s — %s\n\n", in.Name, displayPath)
	n := 0
	last := in.Offset + in.Limit
	truncated := false
	for scanner.Scan() {
		if n >= in.Offset && n < last {
			line := scanner.Text()
			if b.Len()+len(line)+60 > c.MaxOutputBytes-200 {
				b.WriteString("\n[output capped]\n")
				truncated = true
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
		}
		n++
		if n > last {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return SkillReadOut{}, err
	}
	if n > last {
		fmt.Fprintf(&b, "\n(showing lines %d-%d; request offset=%d for more)\n", in.Offset+1, last, last)
		truncated = true
	}
	if !truncated && rel == "SKILL.md" {
		files := listSkillSupportFiles(base, 32)
		if len(files) > 0 {
			index := fmt.Sprintf("\nOther files in this skill, readable with resource=<path>: %s\n", strings.Join(files, ", "))
			if b.Len()+len(index) <= c.MaxOutputBytes {
				b.WriteString(index)
			}
		}
	}
	return SkillReadOut{Content: b.String()}, nil
}

// Bounded read-only package index matching the upstream CodexFree
// skills_read discovery convention; directories/symlinks are not followed.
func listSkillSupportFiles(base string, limit int) []string {
	var files []string
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == base {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || strings.Count(strings.TrimPrefix(path, base), string(os.PathSeparator)) > 4 {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if rel != "SKILL.md" && !strings.HasPrefix(filepath.Base(rel), ".") {
			files = append(files, filepath.ToSlash(rel))
		}
		if len(files) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return files
}
