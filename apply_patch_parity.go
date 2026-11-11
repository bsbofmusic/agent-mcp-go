package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type PatchInput struct {
	Input string `json:"input" jsonschema:"CodexFree-style patch beginning *** Begin Patch and ending *** End Patch."`
	Scope string `json:"scope,omitempty" jsonschema:"Stable task scope for generic clients resuming across connections."`
}

// patchChunk mirrors CodexFree's UpdateChunk: one "@@" section of an Update
// File hunk. changeContext is empty when the hunk carries no "@@ <text>"
// header, isEndOfFile is set by a "*** End of File" marker.
type patchChunk struct {
	changeContext string
	oldLines      []string
	newLines      []string
	isEndOfFile   bool
}

type patchOp struct {
	kind   string
	path   string
	moveTo string
	lines  []string
	chunks []patchChunk
}

type stagedChange struct {
	kind                 string
	path                 string
	moveTo               string
	before               []byte
	after                []byte
	mode                 os.FileMode
	existed              bool
	source, destination  *patchFileRef
	original, committed  patchSnapshot
	temp                 string
	wrote, sourceRemoved bool
}

const (
	patchBegin     = "*** Begin Patch"
	patchEnd       = "*** End Patch"
	patchAddFile   = "*** Add File: "
	patchDelete    = "*** Delete File: "
	patchUpdate    = "*** Update File: "
	patchMoveTo    = "*** Move to: "
	patchEndOfFile = "*** End of File"
)

// isPatchHunkStart reports whether a line starts a new file operation and
// therefore ends the body of the current one.
func isPatchHunkStart(line string) bool {
	return strings.HasPrefix(line, patchAddFile) ||
		strings.HasPrefix(line, patchDelete) ||
		strings.HasPrefix(line, patchUpdate) ||
		line == patchEnd
}

// readCodexPatch parses the CodexFree patch grammar. It is a direct port of
// the locked upstream parser (upstream-audit/codex-free-v1.9.1/src/apply_patch.rs)
// so grammar acceptance, chunk structure and error cases match the reference
// implementation.
func readCodexPatch(patch string) ([]patchOp, error) {
	normalized := strings.ReplaceAll(patch, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	// Tolerate leading/trailing blank lines around the envelope; models
	// routinely add them when the patch is embedded in JSON.
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || lines[0] != patchBegin {
		return nil, fmt.Errorf("The first line of the patch must be '%s'", patchBegin)
	}
	if lines[len(lines)-1] != patchEnd {
		return nil, fmt.Errorf("The last line of the patch must be '%s'", patchEnd)
	}

	ops := []patchOp{}
	i := 1
	for i < len(lines) {
		line := lines[i]
		if line == patchEnd {
			i++
			continue
		}
		if strings.HasPrefix(line, patchAddFile) {
			path := strings.TrimSpace(strings.TrimPrefix(line, patchAddFile))
			i++
			added := []string{}
			for i < len(lines) && !isPatchHunkStart(lines[i]) {
				body := lines[i]
				if !strings.HasPrefix(body, "+") {
					return nil, fmt.Errorf("Unexpected line found in add hunk: '%s'. Every line should start with '+'", body)
				}
				added = append(added, strings.TrimPrefix(body, "+"))
				i++
			}
			ops = append(ops, patchOp{kind: "add", path: path, lines: added})
			continue
		}
		if strings.HasPrefix(line, patchDelete) {
			path := strings.TrimSpace(strings.TrimPrefix(line, patchDelete))
			ops = append(ops, patchOp{kind: "delete", path: path})
			i++
			continue
		}
		if strings.HasPrefix(line, patchUpdate) {
			path := strings.TrimSpace(strings.TrimPrefix(line, patchUpdate))
			i++
			moveTo := ""
			// "*** Move to:" is only a rename when it directly follows the
			// "*** Update File:" header.
			if i < len(lines) && strings.HasPrefix(lines[i], patchMoveTo) {
				moveTo = strings.TrimSpace(strings.TrimPrefix(lines[i], patchMoveTo))
				i++
			}
			chunks := []patchChunk{}
			for i < len(lines) && !isPatchHunkStart(lines[i]) {
				body := lines[i]
				if strings.HasPrefix(body, "@@") {
					chunks = append(chunks, patchChunk{changeContext: strings.TrimSpace(strings.TrimPrefix(body, "@@"))})
					i++
					continue
				}
				if len(chunks) == 0 {
					// The upstream parser is lenient here: an update body that
					// starts before any "@@" header becomes a single
					// context-less chunk rather than an error.
					chunks = append(chunks, patchChunk{})
				}
				chunk := &chunks[len(chunks)-1]
				if body == patchEndOfFile {
					chunk.isEndOfFile = true
					i++
					continue
				}
				switch {
				case strings.HasPrefix(body, "+"):
					chunk.newLines = append(chunk.newLines, strings.TrimPrefix(body, "+"))
				case strings.HasPrefix(body, "-"):
					chunk.oldLines = append(chunk.oldLines, strings.TrimPrefix(body, "-"))
				case body == "":
					// A bare empty line is a context line whose leading space
					// was trimmed.
					chunk.oldLines = append(chunk.oldLines, "")
					chunk.newLines = append(chunk.newLines, "")
				case strings.HasPrefix(body, " "):
					text := strings.TrimPrefix(body, " ")
					chunk.oldLines = append(chunk.oldLines, text)
					chunk.newLines = append(chunk.newLines, text)
				default:
					return nil, fmt.Errorf("Unexpected line found in update hunk: '%s'. Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)", body)
				}
				i++
			}
			if len(chunks) == 0 {
				return nil, fmt.Errorf("Update hunk for '%s' contains no @@ chunks", path)
			}
			ops = append(ops, patchOp{kind: "update", path: path, moveTo: moveTo, chunks: chunks})
			continue
		}
		return nil, fmt.Errorf("Unexpected line found in patch: '%s'. Expected '%s', '%s', '%s' or '%s'",
			line, strings.TrimSpace(patchAddFile), strings.TrimSpace(patchDelete), strings.TrimSpace(patchUpdate), patchEnd)
	}
	if len(ops) == 0 {
		return nil, errors.New("The patch contains no hunks")
	}
	return ops, nil
}

// normalisePatchFuzzy normalises typographic punctuation to ASCII so a patch
// authored with plain quotes still matches source that contains curly ones.
// It is the last resort of the context matching ladder.
func normalisePatchFuzzy(text string) string {
	text = strings.TrimSpace(text)
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r >= '\u2010' && r <= '\u2015', r == '\u2212':
			b.WriteRune('-')
		case r >= '\u2018' && r <= '\u201B':
			b.WriteRune('\'')
		case r >= '\u201C' && r <= '\u201F':
			b.WriteRune('"')
		case r == '\u00A0', r >= '\u2000' && r <= '\u200A', r == '\u202F', r == '\u205F', r == '\u3000':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// patchMatchBudget is shared by every matcher invocation in one patch.
// Remaining counters are injectable by tests, which exercise tiny fixtures.
type patchMatchBudget struct {
	ctx         context.Context
	comparisons int64
	bytes       int64
	ticks       uint64
}

const (
	maxPatchComparisons   int64 = 1 << 20
	maxPatchComparedBytes int64 = 64 << 20
)

var errPatchMatchBudget = errors.New("patch matching work limit exceeded; shrink context or split the patch")

func newPatchMatchBudget(ctx context.Context) *patchMatchBudget {
	return &patchMatchBudget{ctx: ctx, comparisons: maxPatchComparisons, bytes: maxPatchComparedBytes}
}

func (w *patchMatchBudget) use(comparisons, comparedBytes int64) error {
	// Poll at the first operation and every 64 operations afterwards. Charge
	// before comparing or normalizing, so expensive work never runs on debt.
	if w.ticks%64 == 0 {
		if err := w.ctx.Err(); err != nil {
			return err
		}
	}
	w.ticks++
	if comparisons > w.comparisons || comparedBytes > w.bytes {
		return errPatchMatchBudget
	}
	w.comparisons -= comparisons
	w.bytes -= comparedBytes
	return nil
}

func normalizePatchMatchLines(work *patchMatchBudget, lines []string, normalize func(string) string) ([]string, error) {
	result := make([]string, len(lines))
	for i, line := range lines {
		if err := work.use(0, int64(len(line))); err != nil {
			return nil, err
		}
		result[i] = normalize(line)
	}
	return result, nil
}

// seekSequence finds pattern within lines at or after start, trying
// progressively looser comparisons: exact, ignoring trailing whitespace,
// ignoring whitespace on both sides, then with punctuation normalised. When
// eof is set the pattern must match the final position it could occupy.
// It returns the match index or -1.
func seekSequence(lines, pattern []string, start int, eof bool) int {
	index, err := seekSequenceWithMatchBudget(lines, pattern, start, eof, newPatchMatchBudget(context.Background()))
	if err != nil {
		return -1
	}
	return index
}

func seekSequenceWithMatchBudget(lines, pattern []string, start int, eof bool, work *patchMatchBudget) (int, error) {
	if err := work.use(0, 0); err != nil {
		return -1, err
	}
	if len(pattern) == 0 {
		return start, nil
	}
	if len(pattern) > len(lines) {
		return -1, nil
	}
	last := len(lines) - len(pattern)
	searchStart := start
	if eof {
		searchStart = last
	}
	if searchStart < 0 || searchStart > last {
		return -1, nil
	}
	normalizers := []func(string) string{
		nil,
		func(line string) string { return strings.TrimRightFunc(line, unicode.IsSpace) },
		strings.TrimSpace,
		normalisePatchFuzzy,
	}
	for _, normalize := range normalizers {
		candidates, needle := lines[searchStart:], pattern
		if normalize != nil {
			var err error
			// Cache once per pass, rather than rescanning and allocating for
			// every overlapping candidate/pattern pair in the inner loop.
			candidates, err = normalizePatchMatchLines(work, candidates, normalize)
			if err != nil {
				return -1, err
			}
			needle, err = normalizePatchMatchLines(work, needle, normalize)
			if err != nil {
				return -1, err
			}
		}
		for i := 0; i <= last-searchStart; i++ {
			ok := true
			for p, expected := range needle {
				actual := candidates[i+p]
				if err := work.use(1, int64(len(actual))+int64(len(expected))); err != nil {
					return -1, err
				}
				if actual != expected {
					ok = false
					break
				}
			}
			if ok {
				return searchStart + i, nil
			}
		}
	}
	return -1, nil
}

// patchReplacement is one resolved splice of the original file.
type patchReplacement struct {
	start    int
	oldLen   int
	newLines []string
}

// computePatchReplacements resolves every chunk against the original lines.
// Nothing is written here: a chunk that does not match fails the whole patch.
func computePatchReplacements(originalLines []string, path string, chunks []patchChunk) ([]patchReplacement, error) {
	return computePatchReplacementsWithMatchBudget(originalLines, path, chunks, newPatchMatchBudget(context.Background()))
}

func computePatchReplacementsWithMatchBudget(originalLines []string, path string, chunks []patchChunk, work *patchMatchBudget) ([]patchReplacement, error) {
	replacements := []patchReplacement{}
	lineIndex := 0
	for _, chunk := range chunks {
		if err := work.use(0, 0); err != nil {
			return nil, err
		}
		if chunk.changeContext != "" {
			idx, err := seekSequenceWithMatchBudget(originalLines, []string{chunk.changeContext}, lineIndex, false, work)
			if err != nil {
				return nil, err
			}
			if idx < 0 {
				return nil, fmt.Errorf("Failed to find context '%s' in %s", chunk.changeContext, path)
			}
			lineIndex = idx + 1
		}
		if len(chunk.oldLines) == 0 {
			// Pure insertion: append at the end of the file.
			replacements = append(replacements, patchReplacement{
				start:    len(originalLines),
				oldLen:   0,
				newLines: append([]string(nil), chunk.newLines...),
			})
			continue
		}
		pattern := append([]string(nil), chunk.oldLines...)
		newSlice := append([]string(nil), chunk.newLines...)
		found, err := seekSequenceWithMatchBudget(originalLines, pattern, lineIndex, chunk.isEndOfFile, work)
		if err != nil {
			return nil, err
		}
		if found < 0 && len(pattern) > 0 && pattern[len(pattern)-1] == "" {
			// The trailing empty string stands for the file's final newline,
			// which is not a real line. Retry without it.
			pattern = pattern[:len(pattern)-1]
			if len(newSlice) > 0 && newSlice[len(newSlice)-1] == "" {
				newSlice = newSlice[:len(newSlice)-1]
			}
			found, err = seekSequenceWithMatchBudget(originalLines, pattern, lineIndex, chunk.isEndOfFile, work)
			if err != nil {
				return nil, err
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("Failed to find expected lines in %s:\n%s", path, strings.Join(chunk.oldLines, "\n"))
		}
		if found < lineIndex {
			return nil, fmt.Errorf("overlapping or backward patch chunk in %s", path)
		}
		replacements = append(replacements, patchReplacement{
			start:    found,
			oldLen:   len(pattern),
			newLines: newSlice,
		})
		lineIndex = found + len(pattern)
	}
	sort.SliceStable(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	end := 0
	for _, replacement := range replacements {
		if replacement.start < end {
			return nil, fmt.Errorf("overlapping patch ranges in %s", path)
		}
		if replacement.oldLen > 0 {
			end = replacement.start + replacement.oldLen
		}
	}
	return replacements, nil
}

// applyPatchReplacements splices the resolved replacements into the lines.
func applyPatchReplacements(lines []string, replacements []patchReplacement) ([]string, error) {
	result := append([]string(nil), lines...)
	for i := len(replacements) - 1; i >= 0; i-- {
		r := replacements[i]
		if r.start < 0 || r.oldLen < 0 || r.start > len(result) || r.oldLen > len(result)-r.start {
			return nil, errors.New("invalid or overlapping patch replacement range")
		}
		end := r.start + r.oldLen
		merged := make([]string, 0, len(result)-(end-r.start)+len(r.newLines))
		merged = append(merged, result[:r.start]...)
		merged = append(merged, r.newLines...)
		merged = append(merged, result[end:]...)
		result = merged
	}
	return result, nil
}

// usesCRLF reports whether the file predominantly uses CRLF line endings.
func usesCRLF(contents string) bool {
	crlf := strings.Count(contents, "\r\n")
	if crlf == 0 {
		return false
	}
	lf := strings.Count(contents, "\n")
	return crlf*2 >= lf
}

// applyPatchUpdate returns the new contents of a file after applying chunks.
// Line endings are matched to whatever the file already used.
func applyPatchUpdate(original string, chunks []patchChunk, path string) (string, error) {
	return applyPatchUpdateWithin(original, chunks, path, maxPatchWorkingBytes, maxPatchWorkingLines)
}

func applyPatchUpdateWithin(original string, chunks []patchChunk, path string, byteLimit int64, lineLimit int) (string, error) {
	return applyPatchUpdateWithMatchBudget(original, chunks, path, byteLimit, lineLimit, newPatchMatchBudget(context.Background()))
}

func applyPatchUpdateWithMatchBudget(original string, chunks []patchChunk, path string, byteLimit int64, lineLimit int, work *patchMatchBudget) (string, error) {
	if err := work.use(0, 0); err != nil {
		return "", err
	}
	crlf := usesCRLF(original)
	normalized := strings.ReplaceAll(original, "\r\n", "\n")
	originalLines := strings.Split(normalized, "\n")
	// Drop the empty element produced by a trailing newline so line counts
	// match what "diff" would report.
	if len(originalLines) > 0 && originalLines[len(originalLines)-1] == "" {
		originalLines = originalLines[:len(originalLines)-1]
	}
	replacements, err := computePatchReplacementsWithMatchBudget(originalLines, path, chunks, work)
	if err != nil {
		return "", err
	}
	// Account for the rendered output before allocating replacement slices or
	// joining strings. Each rendered line receives its LF (or CRLF).
	lineCount := len(originalLines)
	byteCount := int64(0)
	endingBytes := int64(1)
	if crlf {
		endingBytes = 2
	}
	for _, line := range originalLines {
		byteCount += int64(len(line)) + endingBytes
	}
	for _, r := range replacements {
		lineCount += len(r.newLines) - r.oldLen
		for _, line := range originalLines[r.start : r.start+r.oldLen] {
			byteCount -= int64(len(line)) + endingBytes
		}
		for _, line := range r.newLines {
			byteCount += int64(len(line)) + endingBytes
		}
	}
	if byteCount > byteLimit || lineCount > lineLimit {
		return "", errPatchBudget
	}
	newLines, err := applyPatchReplacements(originalLines, replacements)
	if err != nil {
		return "", err
	}
	newLines = append(newLines, "")
	joined := strings.Join(newLines, "\n")
	if crlf {
		joined = strings.ReplaceAll(joined, "\n", "\r\n")
	}
	return joined, nil
}

// renderAddedFile renders the body of an "*** Add File:" hunk into contents.
func renderAddedFile(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// patchCommitFile is the deterministic commit seam used by fault tests. The
// production implementation validates source/stage versions inside this seam,
// so tests can model a writer changing paths after staging but before commit.
var patchCommitFile = commitPatchChange

// applyCodexPatch preflights all hunks, then stages and commits through pinned
// os.Root parents. Rollback is best-effort and preserves detected external
// versions. Version checks plus Rename/Remove are not a cross-process CAS;
// unrelated writers do not participate in an isolated transaction.
func (a *App) applyCodexPatch(ctx context.Context, input string) (TextOut, error) {
	return a.applyCodexPatchWithMatchBudget(ctx, input, newPatchMatchBudget(ctx))
}

func (a *App) applyCodexPatchWithMatchBudget(ctx context.Context, input string, work *patchMatchBudget) (out TextOut, resultErr error) {
	c := a.config.Get()
	if len(input) == 0 || int64(len(input)) > c.MaxFileBytes {
		return TextOut{}, errors.New("patch exceeds configured file limit")
	}
	if int64(len(input)) > maxPatchWorkingBytes || strings.Count(input, "\n")+1 > maxPatchWorkingLines {
		return TextOut{}, errPatchBudget
	}
	ops, err := readCodexPatch(input)
	if err != nil {
		return TextOut{}, err
	}
	if len(ops) > 100 {
		return TextOut{}, errors.New("more than 100 file operations")
	}
	root, err := os.OpenRoot(c.WorkRoot)
	if err != nil {
		return TextOut{}, err
	}
	parents := &patchParents{work: root, open: map[string]*os.Root{}}
	defer parents.close()
	changes := make([]*stagedChange, 0, len(ops))
	occupied := map[string]bool{}
	workingBytes := int64(len(input))
	workingLines := strings.Count(input, "\n") + 1
	// Lexical validation preserves absolute in-root inputs. Actual filesystem
	// operations always use rooted relative paths; lexical checks alone are
	// not treated as the security boundary.
	relative := func(requested string) (string, error) {
		if requested == "" {
			return "", errors.New("patch file path must not be empty")
		}
		absolute, err := resolveConfigPath(c, requested)
		if err != nil {
			return "", err
		}
		return filepath.Rel(c.WorkRoot, absolute)
	}
	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return TextOut{}, err
		}
		path, err := relative(op.path)
		if err != nil {
			return TextOut{}, err
		}
		if occupied[path] {
			return TextOut{}, fmt.Errorf("duplicate patch target %s", path)
		}
		occupied[path] = true
		ch := &stagedChange{kind: op.kind, path: path, moveTo: path}
		info, statErr := root.Lstat(path)
		if statErr == nil {
			if !info.Mode().IsRegular() {
				return TextOut{}, fmt.Errorf("target is not a regular file: %s", path)
			}
			if info.Size() > c.MaxFileBytes {
				return TextOut{}, errors.New("target exceeds patch size cap")
			}
			ch.source, err = parents.ref(path, false)
			if err != nil {
				return TextOut{}, err
			}
			limit := min(c.MaxFileBytes, maxPatchWorkingBytes-workingBytes)
			ch.original, err = snapshotPatchFile(ch.source, limit)
			if err != nil {
				return TextOut{}, err
			}
			ch.before = ch.original.data
			ch.existed, ch.mode = true, ch.original.info.Mode().Perm()
			workingBytes += int64(len(ch.before))
			workingLines += patchLineCount(ch.before)
			if workingLines > maxPatchWorkingLines {
				return TextOut{}, errPatchBudget
			}
		} else if !os.IsNotExist(statErr) {
			return TextOut{}, statErr
		}
		switch op.kind {
		case "add":
			if ch.existed {
				return TextOut{}, fmt.Errorf("Add File already exists: %s", path)
			}
			// Render only after proving that the payload fits both budgets.
			var size int64
			for _, line := range op.lines {
				size += int64(len(line)) + 1
			}
			if size > c.MaxFileBytes || size > maxPatchWorkingBytes-workingBytes || len(op.lines) > maxPatchWorkingLines-workingLines {
				return TextOut{}, errPatchBudget
			}
			ch.after, ch.mode = []byte(renderAddedFile(op.lines)), 0600
		case "delete":
			if !ch.existed {
				return TextOut{}, fmt.Errorf("Delete File missing: %s", path)
			}
		case "update":
			if !ch.existed {
				return TextOut{}, fmt.Errorf("Update File missing: %s", path)
			}
			if !utf8.Valid(ch.before) {
				return TextOut{}, fmt.Errorf("Update File: %s is not valid UTF-8 text", op.path)
			}
			value, err := applyPatchUpdateWithMatchBudget(string(ch.before), op.chunks, op.path,
				min(c.MaxFileBytes, maxPatchWorkingBytes-workingBytes), maxPatchWorkingLines-workingLines, work)
			if err != nil {
				return TextOut{}, fmt.Errorf("%s: %w", path, err)
			}
			ch.after = []byte(value)
			if op.moveTo != "" {
				dest, err := relative(op.moveTo)
				if err != nil {
					return TextOut{}, err
				}
				if dest == path {
					return TextOut{}, errors.New("move destination equals source")
				}
				if occupied[dest] {
					return TextOut{}, errors.New("duplicate move destination")
				}
				if _, err := root.Lstat(dest); err == nil {
					return TextOut{}, fmt.Errorf("Move destination already exists: %s", dest)
				} else if !os.IsNotExist(err) {
					return TextOut{}, err
				}
				occupied[dest], ch.moveTo = true, dest
			}
		}
		workingBytes += int64(len(ch.after))
		workingLines += patchLineCount(ch.after)
		if workingBytes > maxPatchWorkingBytes || workingLines > maxPatchWorkingLines {
			return TextOut{}, errPatchBudget
		}
		changes = append(changes, ch)
	}
	// All file payloads are staged before any target is modified. Cleanup
	// retains the pinned parent even if the original pathname is redirected.
	defer func() {
		for _, ch := range changes {
			if ch.temp == "" {
				continue
			}
			if err := ch.destination.root.Remove(ch.temp); err != nil && !os.IsNotExist(err) {
				resultErr = errors.Join(resultErr, fmt.Errorf("patch staging cleanup %s: %w", ch.moveTo, err))
			}
		}
	}()
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return TextOut{}, err
		}
		if ch.kind == "delete" {
			continue
		}
		ch.destination, err = parents.ref(ch.moveTo, true)
		if err != nil {
			return TextOut{}, err
		}
		if ch.kind == "add" || ch.moveTo != ch.path {
			if err := ch.destination.absent(); err != nil {
				return TextOut{}, err
			}
		}
		ch.temp, ch.committed, err = stagePatchFile(ch.destination, ch.after, ch.mode)
		if err != nil {
			return TextOut{}, err
		}
	}
	rollback := func(cause error) error {
		var problems []error
		// Include the current partially committed operation (e.g. a Move
		// whose destination was linked but source deletion failed).
		for i := len(changes) - 1; i >= 0; i-- {
			if err := rollbackPatchChange(changes[i]); err != nil {
				problems = append(problems, fmt.Errorf("rollback %s: %w", changes[i].path, err))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("%w (rollback incomplete: %v)", cause, problems)
		}
		return cause
	}
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return TextOut{}, rollback(err)
		}
		if err := patchCommitFile(ch); err != nil {
			return TextOut{}, rollback(err)
		}
	}
	var b strings.Builder
	for _, ch := range changes {
		fmt.Fprintf(&b, "%s %s\n", ch.kind, filepath.Join(c.WorkRoot, ch.path))
	}
	return TextOut{Output: b.String()}, nil
}

func patchLineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := 0
	for _, value := range data {
		if value == '\n' {
			lines++
		}
	}
	if data[len(data)-1] != '\n' {
		lines++
	}
	return lines
}

const patchToolDescription = `Apply a text file patch using this grammar:
*** Begin Patch
*** Add File: path
+new line (every Add body line starts +)
*** Delete File: path
*** Update File: path
*** Move to: new-path (optional, immediately after Update)
@@ or @@ whole-line context anchor
 context line / -removed line / +added line
*** End of File (optional, anchors this chunk at EOF)
*** End Patch
Use one or more file operations; Update accepts multiple @@ chunks (an omitted first @@ is also accepted). Each Update body line starts space, - or +; a bare blank line is empty context. Pure insertion chunks append at EOF, including with an @@ anchor. Context matching tries exact, trailing-whitespace trim, both-side trim, then Unicode quotes/dashes/spaces normalization. Updates retain predominant CRLF/LF and add a final newline. All paths are confined by os.Root. Add/Move use atomic no-clobber. Hunks are preflighted; later failure triggers best-effort rollback that preserves detected external changes and reports rollback incomplete. No cross-file atomicity or isolation from concurrent writers. Limits: 100 operations, configured per-file cap, 16 MiB input+before+after and 100000 cumulative lines; use transfer or smaller patches for larger files. Matching shares a limit of 1048576 line comparisons and 64 MiB of compared/normalized bytes per patch, with periodic cancellation checks; shrink context or split the patch when this work limit is exceeded.`

func (a *App) registerPatchParity(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "apply_patch", Description: patchToolDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in PatchInput) (*mcp.CallToolResult, TextOut, error) {
			out, err := a.applyCodexPatch(ctx, in.Input)
			return nil, out, err
		})
}
