package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// skillCatalogCache memoizes parsed metadata, including an empty catalog. A
// hit compares inode, size, mtime and ctime of every SKILL.md and never opens a
// file; only a changed stat falls back to the bounded frontmatter fingerprint,
// and only a changed fingerprint reparses YAML.
type skillCatalogCache struct {
	mu          sync.Mutex
	valid       bool
	root        string
	fingerprint string
	stats       string
	items       []SkillInfo
	hits        uint64
	parses      uint64
	opens       uint64
}

func (a *App) skillsCacheStats() (hits, parses, opens uint64) {
	cache := &a.skillsCache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.hits, cache.parses, cache.opens
}

const (
	// skillFrontmatterBytes bounds how much of a SKILL.md is read, so an
	// oversized file can never force an unbounded allocation.
	skillFrontmatterBytes = 64 * 1024
)

// readSkillFrontmatter returns the YAML block between the opening and closing
// frontmatter fences, or false when the file does not start with "---" or the
// block is never closed. YAML parsing itself is delegated to yaml.v3 instead of
// hand-rolled string slicing, which broke on quoted colons, escaped quotes and
// block scalars longer than eight lines.
func readSkillFrontmatter(r io.Reader, limit int) (string, bool) {
	// Read one extra byte so truncation at the budget cannot look like EOF.
	br := bufio.NewReader(io.LimitReader(r, int64(limit)+1))
	total := 0
	first, err := br.ReadString('\n')
	total += len(first)
	if total > limit {
		return "", false
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false
	}
	if strings.TrimRight(first, "\r\n") != "---" {
		return "", false
	}
	var body strings.Builder
	for {
		line, err := br.ReadString('\n')
		total += len(line)
		if total > limit {
			return "", false
		}
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

func parseSkillFrontmatterDescription(block string, hasBlock bool) string {
	if !hasBlock {
		return ""
	}
	var fm struct {
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(block), &fm); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(fm.Description), " ")
}

func skillDescription(path string) string {
	f, err := openSkillRegularFile(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return ""
	}
	defer f.Close()
	block, ok := readSkillFrontmatter(f, skillFrontmatterBytes)
	return parseSkillFrontmatterDescription(block, ok)
}

type skillSnapshotEntry struct {
	name     string
	path     string
	block    string
	hasBlock bool
}

// readSkillFrontmatterSnapshot hashes the same bounded blocks used for parsing,
// so the cached metadata always matches the fingerprint, even across edits.
func readSkillFrontmatterSnapshot(root string, entries []os.DirEntry) (string, []skillSnapshotEntry, uint64) {
	h := sha256.New()
	snapshot := make([]skillSnapshotEntry, 0, len(entries))
	var opens uint64
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		base := filepath.Join(root, name)
		f, err := openSkillRegularFile(base, "SKILL.md")
		if err != nil {
			fmt.Fprintf(h, "%q\x00missing/nonregular\n", name)
			continue
		}
		opens++
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			fmt.Fprintf(h, "%q\x00missing/nonregular\n", name)
			continue
		}
		block, ok := readSkillFrontmatter(f, skillFrontmatterBytes)
		_ = f.Close()
		fmt.Fprintf(h, "%q\x00%d\x00%d\x00%t:%x\n", name, fi.ModTime().UnixNano(), fi.Size(), ok, sha256.Sum256([]byte(block)))
		snapshot = append(snapshot, skillSnapshotEntry{
			name: name, path: filepath.Join(base, "SKILL.md"), block: block, hasBlock: ok,
		})
	}
	return hex.EncodeToString(h.Sum(nil)), snapshot, opens
}

// readSkillStatFingerprint hashes the change-relevant metadata of every
// SKILL.md: inode, size, nanosecond mtime and ctime. It only stats, so a cache
// hit costs no file descriptors and no reads. ctime cannot be reset by callers
// and therefore still catches same-size edits whose mtime was restored. A false
// result means the stat view cannot be trusted and the caller must fall back to
// the frontmatter fingerprint.
func readSkillStatFingerprint(root string, entries []os.DirEntry) (string, bool) {
	h := sha256.New()
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		info, err := os.Stat(filepath.Join(root, name, "SKILL.md"))
		if err != nil {
			fmt.Fprintf(h, "%q\x00missing\n", name)
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() {
			return "", false
		}
		fmt.Fprintf(h, "%q\x00%d\x00%d\x00%d\x00%d\n", name, st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano())
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func (a *App) listCanonicalSkills(filter string) (SkillListOut, error) {
	c := a.config.Get()
	root := c.SkillsRoot
	if root == "" {
		// A missing configuration is an operator mistake, not a filesystem
		// error; report it plainly instead of surfacing an os.ReadDir errno.
		return SkillListOut{}, errors.New("skills_root not configured")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return SkillListOut{}, err
	}
	stats, statsOK := readSkillStatFingerprint(root, entries)
	filter = strings.ToLower(filter)
	skills := []SkillInfo{}

	cache := &a.skillsCache
	var items []SkillInfo
	cache.mu.Lock()
	cached := statsOK && cache.valid && cache.root == root && cache.stats == stats
	if cached {
		cache.hits++
		items = make([]SkillInfo, len(cache.items))
		copy(items, cache.items)
	}
	cache.mu.Unlock()

	if !cached {
		fingerprint, snapshot, opens := readSkillFrontmatterSnapshot(root, entries)
		cache.mu.Lock()
		cache.opens += opens
		if cache.valid && cache.root == root && cache.fingerprint == fingerprint {
			// Only file metadata moved (a touched mtime, for example), so the
			// parsed metadata is still current. Refresh the stat snapshot and
			// count this request as a hit.
			cache.hits++
			if statsOK {
				cache.stats = stats
			}
			items = make([]SkillInfo, len(cache.items))
			copy(items, cache.items)
		} else {
			items = make([]SkillInfo, 0, len(snapshot))
			for _, entry := range snapshot {
				items = append(items, SkillInfo{
					Name:        entry.name,
					Description: parseSkillFrontmatterDescription(entry.block, entry.hasBlock),
					Path:        entry.path,
					Scope:       "user",
				})
			}
			cache.valid = true
			cache.root = root
			cache.fingerprint = fingerprint
			if statsOK {
				cache.stats = stats
			}
			cache.items = make([]SkillInfo, len(items))
			copy(cache.items, items)
			cache.parses++
		}
		cache.mu.Unlock()
	}
	for _, item := range items {
		if filter != "" && !strings.Contains(strings.ToLower(item.Name), filter) && !strings.Contains(strings.ToLower(item.Description), filter) {
			continue
		}
		skills = append(skills, item)
	}
	var b strings.Builder
	for _, skill := range skills {
		fmt.Fprintf(&b, "%s — %s\n", skill.Name, skill.Description)
		if b.Len() > c.MaxOutputBytes {
			break
		}
	}
	content := b.String()
	if len(skills) == 0 {
		// A text block is always emitted for this tool, so an empty catalogue
		// or an unmatched search needs a readable explanation rather than an
		// empty string that looks like a transport failure.
		if filter != "" {
			content = "No Skills match the search.\n"
		} else {
			content = "No Skills found in the configured Skills root.\n"
		}
	}
	if clipped, cut := truncateUTF8(content, c.MaxOutputBytes); cut {
		// Step back to a rune boundary so Chinese skill lists never emit
		// invalid UTF-8 when the byte cap lands mid-character.
		content = clipped + "\n[truncated]"
	}
	return SkillListOut{Skills: skills, Content: content}, nil
}

func (a *App) readCanonicalSkill(in SkillReadInput) (SkillReadOut, error) {
	c := a.config.Get()
	if c.SkillsRoot == "" {
		// Same operator-facing message as the listing path; a joined path
		// rooted at "" would otherwise report a confusing errno.
		return SkillReadOut{}, errors.New("skills_root not configured")
	}
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
	if in.Limit < 0 {
		return SkillReadOut{}, errors.New("limit must be nonnegative")
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
	last := in.Offset
	more := false
	for scanner.Scan() {
		if n < in.Offset {
			n++
			continue
		}
		if in.Limit > 0 && n-in.Offset >= in.Limit {
			more = true
			break
		}
		line := scanner.Text()
		if b.Len()+len(line)+128 > c.MaxOutputBytes {
			if last == in.Offset {
				return SkillReadOut{}, errors.New("line exceeds max_output_bytes; raise the budget or use read_file/transfer")
			}
			more = true
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		n++
		last = n
	}
	if err := scanner.Err(); err != nil {
		return SkillReadOut{}, err
	}
	if more {
		fmt.Fprintf(&b, "\n(showing lines %d-%d; request offset=%d for more)\n", in.Offset+1, last, last)
	}
	if !more && rel == "SKILL.md" {
		files, omitted := listSkillSupportFiles(base, max(0, c.MaxOutputBytes-b.Len()-128), c)
		if len(files) > 0 {
			fmt.Fprintf(&b, "\nOther files in this skill, readable with resource=<path>: %s\n", strings.Join(files, ", "))
		}
		if omitted {
			b.WriteString("[Support file index incomplete; use list_directory or glob.]\n")
		}
	}
	return SkillReadOut{Content: b.String()}, nil
}

// Discover regular support files within the response budget; never follow
// symlinks or hidden directories. Reuse the configurable search budgets.
func listSkillSupportFiles(base string, budget int, c Config) ([]string, bool) {
	visited := 0
	var files []string
	omitted := false
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			omitted = true
			return nil
		}
		if path == base {
			return nil
		}
		visited++
		if visited > c.MaxSearchEntries {
			omitted = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(base, path)
			if strings.Count(filepath.ToSlash(rel), "/") >= c.MaxSearchDepth-1 {
				omitted = true
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(base, path)
		if rel == "SKILL.md" || strings.HasPrefix(filepath.Base(rel), ".") {
			return nil
		}
		if len(rel)+2 > budget {
			omitted = true
			return filepath.SkipAll
		}
		files = append(files, filepath.ToSlash(rel))
		budget -= len(rel) + 2
		return nil
	})
	return files, omitted
}
