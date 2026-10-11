package ignore

import (
	"strings"
	"testing"
)

// Job 002c, mục 7 (NIT): a pattern with an empty segment is a parse
// error, not a pattern that silently never matches. Leading/trailing
// slashes are syntax, not empty segments.
func TestIgnore_ParseRejectsEmptySegment(t *testing.T) {
	for _, s := range []string{"a//b", "//x", "a///b"} {
		if _, err := ParseString(s); err == nil {
			t.Errorf("ParseString(%q): expected error, got nil", s)
		}
	}
	_, err := ParseString("ok\nfoo//bar")
	if err == nil {
		t.Fatal("ParseString with empty segment on line 2: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error = %q, want it to name line 2", err)
	}
	for _, s := range []string{"/a", "a/", "**/b", "a/**", "a/**/b"} {
		if _, err := ParseString(s); err != nil {
			t.Errorf("ParseString(%q): err = %v, want nil", s, err)
		}
	}
}
