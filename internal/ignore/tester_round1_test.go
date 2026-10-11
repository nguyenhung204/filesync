package ignore

import "testing"

// Tester round 1: root-anchored patterns beyond the builder's cases —
// anchored dir-only patterns, anchored globs, and anchored patterns
// with '*' that must not cross '/' or match at a deeper level.
func TestTesterIgnore_AnchoredPatterns(t *testing.T) {
	// Dir-only pattern containing '/' is anchored to the sync root.
	m := mustParse(t, "docs/build/\n")
	check(t, m, "docs/build", true, true)
	check(t, m, "docs/build", false, false) // dir-only: the dir itself as file
	check(t, m, "docs/build/x.o", false, true)
	check(t, m, "docs/build/sub/x.o", false, true)
	check(t, m, "sub/docs/build", true, false) // anchored: not at any level
	check(t, m, "sub/docs/build/x.o", false, false)
	check(t, m, "docs/other/x.o", false, false)

	// Leading-slash glob anchors to root and '*' stays in one segment.
	g := mustParse(t, "/docs/*.md\n/*.log\n")
	check(t, g, "docs/a.md", false, true)
	check(t, g, "docs/sub/a.md", false, false) // * must not cross /
	check(t, g, "x/docs/a.md", false, false)   // anchored to root
	check(t, g, "a.log", false, true)
	check(t, g, "x/a.log", false, false) // /*.log only matches at root
	check(t, g, "docs/a.log", false, false)

	// Anchored dir pattern with negation re-include below it.
	n := mustParse(t, "docs/build/\n!docs/build/keep.txt\n")
	check(t, n, "docs/build/keep.txt", false, false)
	check(t, n, "docs/build/drop.o", false, true)
}
