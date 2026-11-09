package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func skillDescription(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), 64*1024)
	if !s.Scan() || strings.TrimSpace(s.Text()) != "---" {
		return ""
	}
	for line := 0; line < 90 && s.Scan(); line++ {
		txt := strings.TrimSpace(s.Text())
		if txt == "---" {
			break
		}
		if !strings.HasPrefix(txt, "description:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(txt, "description:"))
		value = strings.Trim(value, "'\"")
		if value == "|" || value == ">" || value == ">-" || value == "|-" {
			var fragments []string
			for s.Scan() {
				part := s.Text()
				if part != "" && part[0] != ' ' && part[0] != '\t' {
					break
				}
				part = strings.TrimSpace(part)
				if part != "" {
					fragments = append(fragments, part)
				}
				if len(fragments) > 8 {
					break
				}
			}
			value = strings.Join(fragments, " ")
		}
		if len(value) > 700 {
			value = value[:700] + "…"
		}
		return value
	}
	return ""
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
		path := filepath.Join(c.SkillsRoot, ent.Name(), "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		skills = append(skills, SkillInfo{
			Name: ent.Name(), Description: skillDescription(path),
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
	if len(content) > c.MaxOutputBytes {
		content = content[:c.MaxOutputBytes] + "\n[truncated]"
	}
	return SkillListOut{Skills: skills, Content: content}, nil
}

func (a *App) readCanonicalSkill(in SkillReadInput) (SkillReadOut, error) {
	c := a.config.Get()
	if in.Name == "" || in.Name != filepath.Base(in.Name) || strings.Contains(in.Name, "..") {
		return SkillReadOut{}, errors.New("invalid skill name")
	}
	base := filepath.Join(c.SkillsRoot, in.Name)
	resource := in.Resource
	if resource == "" {
		resource = "SKILL.md"
	}
	if filepath.IsAbs(resource) {
		return SkillReadOut{}, errors.New("resource must be relative")
	}
	path := filepath.Clean(filepath.Join(base, resource))
	rel, err := filepath.Rel(base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return SkillReadOut{}, errors.New("resource escapes skill directory")
	}
	// A lexical-only check is insufficient if a support file is a symlink.
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return SkillReadOut{}, err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return SkillReadOut{}, err
	}
	rel, err = filepath.Rel(realBase, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return SkillReadOut{}, errors.New("resource symlink escapes canonical Skill")
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
	f, err := os.Open(realPath)
	if err != nil {
		return SkillReadOut{}, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var b strings.Builder
	// CodexFree skills_read preserves the original Markdown body rather than
	// wrapping every line in a numbered file-view. A numbered YAML frontmatter
	// is not valid YAML and wastes context in newly attached agents.
	fmt.Fprintf(&b, "%s — %s\n\n", in.Name, realPath)
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
	if !truncated && resource == "SKILL.md" {
		files := listSkillSupportFiles(realBase, 32)
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
