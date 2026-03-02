package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helpers

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- atPathsInContent ---

func TestAtPathsInContent_InlineMidSentence(t *testing.T) {
	paths := atPathsInContent("See @notes.md for details.", "/proj/AGENTS.md")
	if len(paths) != 1 || filepath.Base(paths[0]) != "notes.md" {
		t.Errorf("expected [notes.md], got %v", paths)
	}
}

func TestAtPathsInContent_MultipleOnOneLine(t *testing.T) {
	paths := atPathsInContent("Read @file1.md and @file2.md please.", "/proj/AGENTS.md")
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %v", paths)
	}
	if filepath.Base(paths[0]) != "file1.md" || filepath.Base(paths[1]) != "file2.md" {
		t.Errorf("unexpected paths: %v", paths)
	}
}

func TestAtPathsInContent_SkipsFencedBlock(t *testing.T) {
	content := "before\n```\ncode @secret.md\n```\nafter"
	for _, p := range atPathsInContent(content, "/proj/AGENTS.md") {
		if strings.Contains(p, "secret") {
			t.Errorf("should not extract @secret.md from inside fenced block, got %v", p)
		}
	}
}

func TestAtPathsInContent_SkipsFencedBlockWithLanguage(t *testing.T) {
	content := "before\n```go\ncode @secret.md\n```\nafter"
	for _, p := range atPathsInContent(content, "/proj/AGENTS.md") {
		if strings.Contains(p, "secret") {
			t.Errorf("should not extract @secret.md from fenced go block, got %v", p)
		}
	}
}

func TestAtPathsInContent_SkipsInlineCode(t *testing.T) {
	for _, p := range atPathsInContent("Run `@secret.md` to see.", "/proj/AGENTS.md") {
		if strings.Contains(p, "secret") {
			t.Errorf("should not extract @secret.md from inline code, got %v", p)
		}
	}
}

func TestAtPathsInContent_IgnoresEmail(t *testing.T) {
	paths := atPathsInContent("Contact user@example.com for help.", "/proj/AGENTS.md")
	if len(paths) != 0 {
		t.Errorf("expected no paths from email address, got %v", paths)
	}
}

func TestAtPathsInContent_TrailingPunctuation(t *testing.T) {
	cases := []struct{ input, want string }{
		{"See @rules.md.", "rules.md"},
		{"Read @rules.md,", "rules.md"},
		{"Use @rules.md:", "rules.md"},
		{"(@rules.md)", "rules.md"},
	}
	for _, tc := range cases {
		paths := atPathsInContent(tc.input, "/proj/AGENTS.md")
		if len(paths) != 1 || filepath.Base(paths[0]) != tc.want {
			t.Errorf("input %q: expected [%s], got %v", tc.input, tc.want, paths)
		}
	}
}

func TestAtPathsInContent_MarkdownLinkIgnored(t *testing.T) {
	// [@file.md](url) is a Markdown hyperlink — the @ref should not be followed.
	// The "]( " suffix is stripped explicitly before path resolution.
	paths := atPathsInContent("[@rules.md](https://example.com)", "/proj/AGENTS.md")
	for _, p := range paths {
		if strings.Contains(p, "rules") {
			t.Errorf("Markdown hyperlink @ref should not be followed, got %v", p)
		}
	}
}

func TestAtPathsInContent_FragmentStripped(t *testing.T) {
	paths := atPathsInContent("See @file.md#section for more.", "/proj/AGENTS.md")
	if len(paths) != 1 || filepath.Base(paths[0]) != "file.md" {
		t.Errorf("expected [file.md], got %v", paths)
	}
}

func TestAtPathsInContent_AllPathForms(t *testing.T) {
	home, _ := os.UserHomeDir()
	cases := []struct{ input, want string }{
		{"@./rel.md", "/base/rel.md"},
		{"@../up.md", "/up.md"},
		{"@bare.md", "/base/bare.md"},
		{"@/abs/path.md", "/abs/path.md"},
		{"@~/home/notes.md", filepath.Join(home, "home/notes.md")},
	}
	for _, tc := range cases {
		paths := atPathsInContent(tc.input, "/base/AGENTS.md")
		if len(paths) != 1 || paths[0] != tc.want {
			t.Errorf("input %q: expected [%s], got %v", tc.input, tc.want, paths)
		}
	}
}

func TestAtPathsInContent_InsideParens(t *testing.T) {
	paths := atPathsInContent("(see @file.md)", "/proj/AGENTS.md")
	if len(paths) != 1 || filepath.Base(paths[0]) != "file.md" {
		t.Errorf("expected [file.md] from parenthesised ref, got %v", paths)
	}
}

func TestAtPathsInContent_HomeDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	paths := atPathsInContent("@~/notes.md", "/any/AGENTS.md")
	want := filepath.Join(home, "notes.md")
	if len(paths) != 1 || paths[0] != want {
		t.Errorf("expected [%s], got %v", want, paths)
	}
}

func TestAtPathsInContent_RejectsInvalidPaths(t *testing.T) {
	for _, input := range []string{
		"tag @#heading",
		"at sign @@@foo",
	} {
		if paths := atPathsInContent(input, "/proj/AGENTS.md"); len(paths) != 0 {
			t.Errorf("input %q: expected no paths, got %v", input, paths)
		}
	}
}

// --- addGuidanceFile ---

func TestAddGuidanceFile_Basic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Hello @notes.md end")
	writeFile(t, filepath.Join(dir, "notes.md"), "My notes")

	info := &CodebaseInfo{InjectFileContents: make(map[string]string)}
	addGuidanceFile(filepath.Join(dir, "AGENTS.md"), info, make(map[string]bool))

	if len(info.InjectFiles) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(info.InjectFiles), info.InjectFiles)
	}
	if filepath.Base(info.InjectFiles[0]) != "AGENTS.md" {
		t.Errorf("first entry should be AGENTS.md, got %s", info.InjectFiles[0])
	}
	if filepath.Base(info.InjectFiles[1]) != "notes.md" {
		t.Errorf("second entry should be notes.md, got %s", info.InjectFiles[1])
	}
	if info.InjectFileContents[info.InjectFiles[1]] != "My notes" {
		t.Errorf("unexpected content for notes.md: %q", info.InjectFileContents[info.InjectFiles[1]])
	}
}

func TestAddGuidanceFile_CircularIgnored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "I am A, see @b.md")
	writeFile(t, filepath.Join(dir, "b.md"), "I am B, see @a.md")

	info := &CodebaseInfo{InjectFileContents: make(map[string]string)}
	addGuidanceFile(filepath.Join(dir, "a.md"), info, make(map[string]bool))

	if len(info.InjectFiles) != 2 {
		t.Fatalf("expected 2 files (no infinite loop), got %d", len(info.InjectFiles))
	}
	seen := map[string]int{}
	for _, f := range info.InjectFiles {
		seen[f]++
	}
	for path, count := range seen {
		if count > 1 {
			t.Errorf("%s loaded %d times, expected 1", path, count)
		}
	}
}

func TestAddGuidanceFile_ChainFollowed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "I am A, see @b.md")
	writeFile(t, filepath.Join(dir, "b.md"), "I am B, see @c.md")
	writeFile(t, filepath.Join(dir, "c.md"), "I am C")

	info := &CodebaseInfo{InjectFileContents: make(map[string]string)}
	addGuidanceFile(filepath.Join(dir, "a.md"), info, make(map[string]bool))

	if len(info.InjectFiles) != 3 {
		t.Fatalf("expected 3 files in chain, got %d", len(info.InjectFiles))
	}
	for i, want := range []string{"a.md", "b.md", "c.md"} {
		if filepath.Base(info.InjectFiles[i]) != want {
			t.Errorf("position %d: expected %s, got %s", i, want, filepath.Base(info.InjectFiles[i]))
		}
	}
}

func TestAddGuidanceFile_MissingRefSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "See @nonexistent.md for details")

	info := &CodebaseInfo{InjectFileContents: make(map[string]string)}
	addGuidanceFile(filepath.Join(dir, "AGENTS.md"), info, make(map[string]bool))

	if len(info.InjectFiles) != 1 {
		t.Errorf("expected 1 file (missing ref skipped), got %d: %v", len(info.InjectFiles), info.InjectFiles)
	}
}

func TestAddGuidanceFile_Deduplication(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "A imports @shared.md")
	writeFile(t, filepath.Join(dir, "b.md"), "B imports @shared.md")
	writeFile(t, filepath.Join(dir, "shared.md"), "I am shared")

	info := &CodebaseInfo{InjectFileContents: make(map[string]string)}
	seen := make(map[string]bool)
	addGuidanceFile(filepath.Join(dir, "a.md"), info, seen)
	addGuidanceFile(filepath.Join(dir, "b.md"), info, seen)

	// a.md + shared.md + b.md = 3; shared.md must appear exactly once
	if len(info.InjectFiles) != 3 {
		t.Fatalf("expected 3 files, got %d: %v", len(info.InjectFiles), info.InjectFiles)
	}
	count := 0
	for _, f := range info.InjectFiles {
		if filepath.Base(f) == "shared.md" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared.md should appear exactly once, appeared %d times", count)
	}
}

// --- integration ---

func TestSystemPrompt_AtFileIncluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Rules: see @extra.md for more.")
	writeFile(t, filepath.Join(dir, "extra.md"), "EXTRA_UNIQUE_CONTENT_99887")

	prompt, err := GenerateSystemPrompt(dir)
	if err != nil {
		t.Fatalf("GenerateSystemPrompt: %v", err)
	}
	if !strings.Contains(prompt, "EXTRA_UNIQUE_CONTENT_99887") {
		t.Error("imported file content should appear in the system prompt")
	}
	if !strings.Contains(prompt, filepath.Join(dir, "extra.md")) {
		t.Error("imported file path should appear in the system prompt")
	}
}
