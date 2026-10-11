package cli

import (
	"strings"
	"testing"
)

// psOutsideLiterals returns the parts of a PowerShell payload
// that are NOT inside single-quoted string literals ('' inside a
// literal is an escaped quote, per PowerShell rules). Anything
// dangerous in the output is live code; anything inside a literal
// is inert data.
func psOutsideLiterals(payload string) string {
	var b strings.Builder
	inStr := false
	for i := 0; i < len(payload); i++ {
		c := payload[i]
		if c == '\'' {
			if inStr && i+1 < len(payload) && payload[i+1] == '\'' {
				i++ // escaped quote, still inside the literal
				continue
			}
			inStr = !inStr
			continue
		}
		if !inStr {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func TestSvcElevationPayload(t *testing.T) {
	self := `C:\Users\A B\AppData\Local\Programs\filesync\filesync.exe`
	payload := svcElevationPayload(self)
	for _, want := range []string{
		"Start-Process", "-FilePath '" + self + "'", "-Verb RunAs",
		"-Wait", "-ArgumentList 'uninstall-service --elevated'",
		"try {", "catch { exit 1 }", "if ($null -eq $p) { exit 1 }",
		"exit $p.ExitCode",
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %q missing %q", payload, want)
		}
	}
	// The exe path must never sit in a double-quoted PowerShell
	// string: $(...) and backtick escapes still execute there.
	if strings.Contains(payload, `-FilePath "`) {
		t.Errorf("exe path double-quoted in payload: %q", payload)
	}
}

func TestSvcElevationPayloadEvilPath(t *testing.T) {
	// Same hostile folder name as the setup-side test: $, parens,
	// backtick and apostrophe are all legal in a Windows path and
	// must stay inert data inside a single-quoted literal.
	evil := "C:\\Users\\$(calc) O'Neil\\`x\\filesync.exe"
	payload := svcElevationPayload(evil)
	if !strings.Contains(payload, "-FilePath "+svcPsQuote(evil)) {
		t.Errorf("evil path not svcPsQuote-wrapped: %q", payload)
	}
	out := psOutsideLiterals(payload)
	for _, bad := range []string{"$(", "`", "calc", "filesync.exe"} {
		if strings.Contains(out, bad) {
			t.Errorf("payload fragment %q leaked outside string literals: %q (outside: %q)", bad, payload, out)
		}
	}
}

func TestSvcPsQuote(t *testing.T) {
	if got := svcPsQuote("a'b"); got != `'a''b'` {
		t.Errorf("svcPsQuote = %q", got)
	}
	if got := svcPsQuote(`C:\plain\path`); got != `'C:\plain\path'` {
		t.Errorf("svcPsQuote = %q", got)
	}
}
