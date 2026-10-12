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

const svcQueryexStartPending = `
SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 2  START_PENDING 
        WIN32_EXIT_CODE    : 0  (0x0)
        CHECKPOINT         : 0x1
        WAIT_HINT          : 0x7d0
        PID                : 6724
        FLAGS              :
`

const svcQueryexStopped = `
SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 1  STOPPED 
        WIN32_EXIT_CODE    : 0  (0x0)
        CHECKPOINT         : 0x0
        WAIT_HINT          : 0x0
        PID                : 0
        FLAGS              :
`

const svcQueryNotInstalled = `[SC] QueryService FAILED 1060:

The specified service does not exist as an installed service.
`

// The CLI-side copies of the D4 helpers must behave exactly
// like the cmd/filesync-setup originals.
func TestSvcParseScQueryState(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"start pending", svcQueryexStartPending, "START_PENDING"},
		{"stopped", svcQueryexStopped, "STOPPED"},
		{"not installed", svcQueryNotInstalled, "NOT_INSTALLED"},
		{"garbage", "blah\n", "UNKNOWN"},
		{"empty", "", "UNKNOWN"},
	}
	for _, c := range cases {
		if got := svcParseScQueryState(c.in); got != c.want {
			t.Errorf("%s: svcParseScQueryState = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSvcParseScQueryexPID(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"wedged service with process", svcQueryexStartPending, 6724, true},
		{"stopped service has pid 0", svcQueryexStopped, 0, true},
		{"not installed (1060)", svcQueryNotInstalled, 0, false},
		{"garbage", "blah blah\n", 0, false},
		{"pid not a number", "        PID                : abc\n", 0, false},
		{"mid-line PID is not the field", "        FLAGS              : PID : 999\n", 0, false},
		{"decoy line before real pid", "        SOME_PID           : 999\n        PID                : 6724\n", 6724, true},
	}
	for _, c := range cases {
		got, ok := svcParseScQueryexPID(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: svcParseScQueryexPID = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestSvcStuckAction(t *testing.T) {
	cases := []struct {
		state string
		want  svcStuckDecision
	}{
		{"STOPPED", svcStuckProceed},
		{"START_PENDING", svcStuckKillThenDelete},
		{"STOP_PENDING", svcStuckKillThenDelete},
		{"PAUSE_PENDING", svcStuckKillThenDelete},
		{"CONTINUE_PENDING", svcStuckKillThenDelete},
		{"RUNNING", svcStuckFail},
		{"PAUSED", svcStuckFail},
		{"UNKNOWN", svcStuckFail},
		{"", svcStuckFail},
	}
	for _, c := range cases {
		if got := svcStuckAction(c.state); got != c.want {
			t.Errorf("svcStuckAction(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestSvcTaskkillArgs(t *testing.T) {
	got := svcTaskkillArgs(6724)
	want := []string{"/PID", "6724", "/F"}
	if len(got) != len(want) {
		t.Fatalf("svcTaskkillArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("svcTaskkillArgs[%d] = %q, want %q", i, got[i], want[i])
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
