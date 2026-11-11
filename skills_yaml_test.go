package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeSkillMarkdown(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write SKILL.md fixture: %v", err)
	}
	return path
}

func blockScalarSkillMD(indicator string, lines []string) string {
	var b strings.Builder
	b.WriteString("---\nname: demo\ndescription: ")
	b.WriteString(indicator)
	b.WriteString("\n")
	for _, line := range lines {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	return b.String()
}

// Quote styles, escaped quotes and colons inside plain/quoted scalars are all
// legal YAML that the old string-slicing parser mangled.
func TestSkillDescriptionQuotedAndColonValues(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "plain scalar with url colon",
			body: "---\nname: demo\ndescription: Serve http://example.com:8080 docs\n---\n",
			want: "Serve http://example.com:8080 docs",
		},
		{
			name: "plain scalar continuation folds",
			body: "---\ndescription: first line\n  second line\n---\n",
			want: "first line second line",
		},
		{
			name: "single quoted with colon and doubled quote",
			body: "---\ndescription: 'Read: write files. It''s fast.'\n---\n",
			want: "Read: write files. It's fast.",
		},
		{
			name: "double quoted with colon and escaped quotes",
			body: "---\ndescription: \"Read: files with \\\"double quotes\\\" inside.\"\n---\n",
			want: `Read: files with "double quotes" inside.`,
		},
		{
			name: "whitespace runs collapse",
			body: "---\ndescription: \"alpha\\t beta\\n\\n  gamma\"\n---\n",
			want: "alpha beta gamma",
		},
		{
			name: "crlf frontmatter",
			body: "---\r\ndescription: crlf works\r\n---\r\n",
			want: "crlf works",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := skillDescription(writeSkillMarkdown(t, tc.body))
			if got != tc.want {
				t.Fatalf("skillDescription() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Block scalars longer than eight lines used to be cut off by the hand-rolled
// fragment loop; YAML block scalars must be read to the closing fence.
func TestSkillDescriptionBlockScalarsBeyondEightLines(t *testing.T) {
	lines := make([]string, 0, 12)
	wantParts := make([]string, 0, 12)
	for i := 1; i <= 12; i++ {
		chunk := fmt.Sprintf("chunk %02d", i)
		lines = append(lines, chunk)
		wantParts = append(wantParts, chunk)
	}
	want := strings.Join(wantParts, " ")
	for _, indicator := range []string{">", ">-", "|", "|-"} {
		t.Run("indicator_"+indicator, func(t *testing.T) {
			got := skillDescription(writeSkillMarkdown(t, blockScalarSkillMD(indicator, lines)))
			if got != want {
				t.Fatalf("skillDescription() for %q = %q, want %q", indicator, got, want)
			}
		})
	}
}

// A >900 byte Chinese description must be truncated on a rune boundary: byte
// 700 lands mid-character for three-byte runes.
func TestSkillDescriptionLongChineseStaysValidUTF8(t *testing.T) {
	long := strings.Repeat("技能描述", 210) // 210 * 12 = 2520 bytes
	body := "---\nname: long\ndescription: " + long + "\n---\n"
	got := skillDescription(writeSkillMarkdown(t, body))
	if len(got) > skillDescriptionBytes+len(skillDescriptionTail) {
		t.Fatalf("len(got) = %d, want <= %d", len(got), skillDescriptionBytes+len(skillDescriptionTail))
	}
	if len(got) <= skillDescriptionBytes {
		t.Fatalf("expected truncation at the byte cap, got %d bytes", len(got))
	}
	if !strings.HasSuffix(got, skillDescriptionTail) {
		t.Fatalf("long description missing %q suffix: %q", skillDescriptionTail, got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncated description is not valid UTF-8: %q", got)
	}
}

func TestTruncateUTF8StepsBackToRuneBoundary(t *testing.T) {
	s := strings.Repeat("技", 300) // 900 bytes, three bytes per rune
	cut, truncated := truncateUTF8(s, 700)
	if !truncated {
		t.Fatal("expected truncateUTF8 to report truncation")
	}
	if len(cut) != 699 {
		t.Fatalf("len(cut) = %d, want 699 (byte 700 lands mid-rune)", len(cut))
	}
	if !utf8.ValidString(cut) {
		t.Fatal("cut string is not valid UTF-8")
	}
	if got, truncated := truncateUTF8("short", 700); truncated || got != "short" {
		t.Fatalf("no-op truncation changed %q to %q (truncated=%v)", "short", got, truncated)
	}
}

func TestSkillDescriptionFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing opening fence", "name: demo\ndescription: hidden\n---\n"},
		{"unclosed frontmatter", "---\nname: demo\ndescription: hidden\n"},
		{"invalid yaml", "---\ndescription: \"unterminated\n---\n"},
		{"missing description key", "---\nname: demo\n---\n"},
		{"description is a mapping", "---\ndescription:\n  nested: value\n---\n"},
		{"empty file", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := skillDescription(writeSkillMarkdown(t, tc.body)); got != "" {
				t.Fatalf("skillDescription() = %q, want empty string", got)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if got := skillDescription(filepath.Join(t.TempDir(), "SKILL.md")); got != "" {
			t.Fatalf("skillDescription(missing) = %q, want empty string", got)
		}
	})
}

func TestSkillDescriptionAcceptsDotsTerminator(t *testing.T) {
	got := skillDescription(writeSkillMarkdown(t, "---\ndescription: dotted end\n...\n"))
	if got != "dotted end" {
		t.Fatalf("skillDescription() with %q terminator = %q, want %q", "...", got, "dotted end")
	}
}

// The skill-list byte cap must never split a Chinese character either.
func TestListCanonicalSkillsTruncatesOnRuneBoundary(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "技能")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	description := strings.Repeat("技能", 200) // 1200 bytes, capped at 702
	body := "---\nname: 技能\ndescription: " + description + "\n---\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	app := &App{config: &ConfigStore{cfg: Config{SkillsRoot: root, MaxOutputBytes: 400}}}
	out, err := app.listCanonicalSkills("")
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out.Content) {
		t.Fatalf("truncated skill list is not valid UTF-8: %q", out.Content)
	}
	if !strings.HasSuffix(out.Content, "\n[truncated]") {
		t.Fatalf("expected truncation marker, got %q", out.Content)
	}
	if len(out.Content) > 400+len("\n[truncated]") {
		t.Fatalf("len(content) = %d, want <= %d", len(out.Content), 400+len("\n[truncated]"))
	}
}
