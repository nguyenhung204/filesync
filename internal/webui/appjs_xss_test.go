package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Regression tests for the round-1 security finding: esc() in
// static/app.js did not escape the single quote, while untrusted
// values (above all the self-declared peer name) are interpolated
// into single-quoted HTML attributes (data-name='...', data-revoke,
// <option value='...'>). A peer named
//
//	' onclick='fetch("/api/pause",{method:"POST"})//
//
// broke out of the attribute and injected an onclick handler that
// runs with full UI-origin privileges when the owner clicks the
// peer's Revoke button.
//
// There is no JS engine in the Go toolchain, so the first test
// extracts the actual .replace() chain from the shipped esc() and
// replays it verbatim in Go — it exercises the exact escaping
// logic the browser will run, not a copy of it. The second test
// scans app.js so every value interpolated into a single-quoted
// attribute is forced through esc().

func readAppJS(t *testing.T) string {
	t.Helper()
	raw, err := fs.ReadFile(staticFS, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

var escStepRe = regexp.MustCompile(`\.replace\(/(.)/g,\s*"([^"]*)"\)`)

// escChain parses the ordered single-char replacements of esc()
// out of app.js.
func escChain(t *testing.T, src string) [][2]string {
	t.Helper()
	start := strings.Index(src, "function esc(")
	if start < 0 {
		t.Fatal("esc() not found in app.js")
	}
	body := src[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	var chain [][2]string
	for _, m := range escStepRe.FindAllStringSubmatch(body, -1) {
		chain = append(chain, [2]string{m[1], m[2]})
	}
	if len(chain) == 0 {
		t.Fatal("no .replace() steps parsed from esc()")
	}
	return chain
}

// replayEsc applies esc()'s chain to s in source order, exactly as
// the browser would.
func replayEsc(chain [][2]string, s string) string {
	for _, step := range chain {
		s = strings.ReplaceAll(s, step[0], step[1])
	}
	return s
}

func TestEscNeutralizesAttributeBreakout(t *testing.T) {
	chain := escChain(t, readAppJS(t))
	payloads := []string{
		// The reviewer's proof-of-concept peer name.
		`' onclick='fetch("/api/pause",{method:"POST"})//`,
		`x' data-evil='1`,
		`a"b<c>d&e'f`,
		`<script>alert('xss')</script>`,
		`bình thường ' có dấu ' nháy`,
	}
	entities := []string{"&amp;", "&lt;", "&gt;", "&quot;", "&#39;"}
	for _, p := range payloads {
		out := replayEsc(chain, p)
		for _, raw := range []string{"'", `"`, "<", ">"} {
			if strings.Contains(out, raw) {
				t.Errorf("esc(%q) = %q: still contains raw %q — attribute breakout possible", p, out, raw)
			}
		}
		// Every & left in the output must open a known entity.
		rest := out
		for _, e := range entities {
			rest = strings.ReplaceAll(rest, e, "")
		}
		if strings.Contains(rest, "&") {
			t.Errorf("esc(%q) = %q: bare & outside a known entity", p, out)
		}
	}
	// The attack payload only stays safe if its quotes were
	// actually escaped, not stripped.
	if out := replayEsc(chain, payloads[0]); !strings.Contains(out, "&#39;") {
		t.Errorf("esc(attack payload) = %q: single quote was not escaped to &#39;", out)
	}
}

// attrInterpRe matches the app.js idiom for interpolating a value
// into a single-quoted attribute: '" + expr + "'
var attrInterpRe = regexp.MustCompile(`'"\s*\+\s*(.+?)\s*\+\s*"'`)

func TestAppJSAttributeInterpolationsAreEscaped(t *testing.T) {
	src := readAppJS(t)
	// Numeric-only expressions that cannot carry attacker data:
	// "i" is the JS map index used for data-i in the conflicts tab.
	allowed := map[string]bool{"i": true}
	matches := attrInterpRe.FindAllStringSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatal("no attribute interpolations found — regex out of sync with app.js?")
	}
	for _, m := range matches {
		expr := strings.TrimSpace(m[1])
		if strings.HasPrefix(expr, "esc(") || allowed[expr] {
			continue
		}
		t.Errorf("attribute interpolation %q is not passed through esc()", m[0])
	}
}
