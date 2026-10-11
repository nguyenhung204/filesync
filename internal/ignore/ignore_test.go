package ignore

import (
	"path/filepath"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) *Matcher {
	t.Helper()
	m, err := ParseString(s)
	if err != nil {
		t.Fatalf("ParseString(%q): %v", s, err)
	}
	return m
}

func check(t *testing.T, m *Matcher, path string, isDir bool, want bool) {
	t.Helper()
	if got := m.Match(path, isDir); got != want {
		t.Errorf("Match(%q, isDir=%v) = %v, want %v", path, isDir, got, want)
	}
}

func TestIgnore_CommentAndBlankLines(t *testing.T) {
	m := mustParse(t, "# comment\n\n   \n\\#literal\nreal.txt\n")
	check(t, m, "#literal", false, true)
	check(t, m, "real.txt", false, true)
	check(t, m, "other.txt", false, false)
	check(t, m, "# comment", false, false)
}

func TestIgnore_BasenamePatternAnyLevel(t *testing.T) {
	m := mustParse(t, "*.log\n")
	check(t, m, "a.log", false, true)
	check(t, m, "x/y/a.log", false, true)
	check(t, m, "a.logx", false, false)
	check(t, m, "x/log.txt", false, false)
}

func TestIgnore_NegationLastMatchWins(t *testing.T) {
	m := mustParse(t, "*.log\n!keep.log\n")
	check(t, m, "keep.log", false, false)
	check(t, m, "x/keep.log", false, false)
	check(t, m, "x.log", false, true)

	reversed := mustParse(t, "!keep.log\n*.log\n")
	check(t, reversed, "keep.log", false, true)
}

func TestIgnore_DirPattern(t *testing.T) {
	m := mustParse(t, "build/\n!build/keep.txt\n")
	check(t, m, "build", false, false)
	check(t, m, "build", true, true)
	check(t, m, "build/x.o", false, true)
	check(t, m, "build/sub/x.o", false, true)
	check(t, m, "build/keep.txt", false, false)
	check(t, m, "src/build", true, true) // unanchored: any level
	check(t, m, "building", true, false)
}

func TestIgnore_AnchoredPattern(t *testing.T) {
	m := mustParse(t, "/root.txt\ndocs/draft.md\n")
	check(t, m, "root.txt", false, true)
	check(t, m, "sub/root.txt", false, false)
	check(t, m, "docs/draft.md", false, true)
	check(t, m, "x/docs/draft.md", false, false)

	unanchored := mustParse(t, "draft.md\n")
	check(t, unanchored, "x/y/draft.md", false, true)
}

func TestIgnore_DoubleStar(t *testing.T) {
	m := mustParse(t, "**/tmp/**\n")
	check(t, m, "tmp/a.txt", false, true)
	check(t, m, "a/b/tmp/c/d.txt", false, true)
	check(t, m, "a/temp/c.txt", false, false)

	mid := mustParse(t, "a/**/b\n")
	check(t, mid, "a/b", false, true) // ** matches zero segments
	check(t, mid, "a/x/b", false, true)
	check(t, mid, "a/x/y/b", false, true)
	check(t, mid, "a/x/y/c", false, false)
	check(t, mid, "z/a/b", false, false) // anchored

	all := mustParse(t, "**\n")
	check(t, all, "anything/at/all.txt", false, true)
	check(t, all, "one", false, true)

	under := mustParse(t, "x/**\n")
	check(t, under, "x/y", false, true)
	check(t, under, "x/y/z", false, true)
	check(t, under, "x", true, false) // not x itself
	check(t, under, "x", false, false)
}

func TestIgnore_GlobChars(t *testing.T) {
	m := mustParse(t, "file?.txt\n[a-c].txt\n")
	check(t, m, "file1.txt", false, true)
	check(t, m, "file12.txt", false, false)
	check(t, m, "b.txt", false, true)
	check(t, m, "d.txt", false, false)

	_, err := ParseString("ok.txt\n[abc\n")
	if err == nil {
		t.Fatal("bad glob: expected parse error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("parse error should carry line number, got: %v", err)
	}
}

func TestIgnore_LoadFileMissing(t *testing.T) {
	m, err := LoadFile(filepath.Join(t.TempDir(), "no-such-file"))
	if err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if m == nil {
		t.Fatal("missing file: matcher is nil")
	}
	check(t, m, "anything.log", false, false)
}

func TestIgnore_NilMatcher(t *testing.T) {
	var m *Matcher
	if m.Match("a.log", false) {
		t.Fatal("nil matcher must not match")
	}
	if m.Match("", false) {
		t.Fatal("nil matcher must not match empty path")
	}
}

func TestIgnore_EscapedBangAndCRLF(t *testing.T) {
	m := mustParse(t, "\\!important.txt\r\n*.tmp\r\n")
	check(t, m, "!important.txt", false, true)
	check(t, m, "x.tmp", false, true)
}
