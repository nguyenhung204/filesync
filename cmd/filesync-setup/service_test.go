package main

import (
	"path/filepath"
	"strings"
	"testing"
)

const scQueryRunning = `

SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 4  RUNNING 
                                (STOPPABLE, NOT_PAUSABLE, ACCEPTS_SHUTDOWN)
        WIN32_EXIT_CODE    : 0  (0x0)
        SERVICE_EXIT_CODE  : 0  (0x0)
        CHECKPOINT         : 0x0
        WAIT_HINT          : 0x0
`

const scQueryStopped = `
SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 1  STOPPED 
        WIN32_EXIT_CODE    : 0  (0x0)
`

const scQueryStartPending = `
SERVICE_NAME: FileSync 
        STATE              : 2  START_PENDING 
                                (NOT_STOPPABLE, NOT_PAUSABLE, IGNORES_SHUTDOWN)
`

const scQueryNotInstalled = `[SC] QueryService FAILED 1060:

The specified service does not exist as an installed service.
`

func TestParseScQueryState(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"running", scQueryRunning, "RUNNING"},
		{"stopped", scQueryStopped, "STOPPED"},
		{"start pending", scQueryStartPending, "START_PENDING"},
		{"not installed", scQueryNotInstalled, "NOT_INSTALLED"},
		{"garbage", "blah blah\nnothing useful\n", "UNKNOWN"},
		{"empty", "", "UNKNOWN"},
	}
	for _, c := range cases {
		if got := parseScQueryState(c.in); got != c.want {
			t.Errorf("%s: parseScQueryState = %q, want %q", c.name, got, c.want)
		}
	}
}

const initFresh = `initialized: device K57JFIRQAJM5Y2N54ISHGE5LWSSTKGUIRV3FEIPMLOB7BDO6WYBA
device_id:    K57JFIRQAJM5Y2N54ISHGE5LWSSTKGUIRV3FEIPMLOB7BDO6WYBA
fingerprint:  577E 92A2 BEEF 1234
config_dir:   C:\Users\Maxsys\AppData\Roaming\filesync
sync_dir:     C:\Users\Maxsys\FileSync
listen_addr:  :22000
`

const initRerun = `already initialized: device K57JFIRQAJM5Y2N54ISHGE5LWSSTKGUIRV3FEIPMLOB7BDO6WYBA
device_id:    K57JFIRQAJM5Y2N54ISHGE5LWSSTKGUIRV3FEIPMLOB7BDO6WYBA
fingerprint:  577E 92A2 BEEF 1234
config_dir:   C:\Users\Maxsys\AppData\Roaming\filesync
sync_dir:     C:\Users\Maxsys\FileSync
listen_addr:  :22000
`

func TestParseInitOutput(t *testing.T) {
	for _, in := range []string{initFresh, initRerun} {
		info := parseInitOutput(in)
		if info.DeviceID != "K57JFIRQAJM5Y2N54ISHGE5LWSSTKGUIRV3FEIPMLOB7BDO6WYBA" {
			t.Errorf("DeviceID = %q", info.DeviceID)
		}
		if info.Fingerprint != "577E 92A2 BEEF 1234" {
			t.Errorf("Fingerprint = %q", info.Fingerprint)
		}
		if info.SyncDir != `C:\Users\Maxsys\FileSync` {
			t.Errorf("SyncDir = %q", info.SyncDir)
		}
		if info.ConfigDir != `C:\Users\Maxsys\AppData\Roaming\filesync` {
			t.Errorf("ConfigDir = %q", info.ConfigDir)
		}
	}
	if info := parseInitOutput("some unrelated output\n"); info.DeviceID != "" || info.Fingerprint != "" {
		t.Errorf("missing lines should stay empty, got %+v", info)
	}
	if info := parseInitOutput(""); info != (initInfo{}) {
		t.Errorf("empty output should give zero info, got %+v", info)
	}
}

func TestScCreateArgsWithSpacedPath(t *testing.T) {
	exe := `C:\Users\A B\AppData\Local\Programs\filesync\filesync.exe`
	cfg := `C:\Users\A B\AppData\Roaming\filesync`
	args := scCreateArgs(exe, cfg)
	want := []string{
		"create", "FileSync",
		"binPath=", `"` + exe + `" serve --config-dir "` + cfg + `"`,
		"start=", "auto",
		"DisplayName=", "FileSync",
	}
	if len(args) != len(want) {
		t.Fatalf("scCreateArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	// The binPath value must keep its embedded quotes so the
	// service command line survives paths with spaces.
	if !strings.Contains(args[3], `" serve --config-dir "`) {
		t.Errorf("binPath value lost quoting: %q", args[3])
	}
}

func TestScOtherArgs(t *testing.T) {
	if got := strings.Join(scFailureArgs(), " "); got != "failure FileSync reset= 86400 actions= restart/5000" {
		t.Errorf("scFailureArgs = %q", got)
	}
	if got := strings.Join(scDescriptionArgs(), " "); !strings.HasPrefix(got, "description FileSync ") {
		t.Errorf("scDescriptionArgs = %q", got)
	}
	for name, args := range map[string][]string{
		"query": scQueryArgs(), "stop": scStopArgs(),
		"delete": scDeleteArgs(), "start": scStartArgs(),
	} {
		if len(args) != 2 || args[1] != "FileSync" {
			t.Errorf("%s args = %v", name, args)
		}
	}
}

func TestComputePaths(t *testing.T) {
	p := computePaths(`C:\Users\X\AppData\Local`, `C:\Users\X\AppData\Roaming`)
	if filepath.Base(p.installDir) != "filesync" || filepath.Base(filepath.Dir(p.installDir)) != "Programs" {
		t.Errorf("installDir = %q", p.installDir)
	}
	if filepath.Base(p.exe) != "filesync.exe" || filepath.Dir(p.exe) != p.installDir {
		t.Errorf("exe = %q", p.exe)
	}
	if filepath.Base(p.configDir) != "filesync" || !strings.HasSuffix(p.shortcutPath, "FileSync.lnk") {
		t.Errorf("configDir=%q shortcut=%q", p.configDir, p.shortcutPath)
	}
}

func TestRegAddCommands(t *testing.T) {
	p := computePaths(`C:\Users\X\AppData\Local`, `C:\Users\X\AppData\Roaming`)
	cmds := regAddCommands(p)
	if len(cmds) != 7 {
		t.Fatalf("expected 7 reg add commands, got %d", len(cmds))
	}
	byName := map[string][]string{}
	for _, c := range cmds {
		if c[0] != "add" || c[1] != uninstallKey || c[len(c)-1] != "/f" {
			t.Errorf("malformed reg add argv: %v", c)
		}
		byName[c[3]] = c
	}
	// args layout: add <key> /v <name> /t <type> /d <data> /f
	get := func(name string) (typ, data string) {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("missing registry value %s", name)
		}
		return c[5], c[7]
	}
	if typ, data := get("DisplayName"); typ != "REG_SZ" || data != "FileSync" {
		t.Errorf("DisplayName = %s %q", typ, data)
	}
	if _, data := get("DisplayVersion"); data != setupVersion {
		t.Errorf("DisplayVersion = %q, want setupVersion %q", data, setupVersion)
	}
	if _, data := get("InstallLocation"); data != p.installDir {
		t.Errorf("InstallLocation = %q", data)
	}
	if _, data := get("DisplayIcon"); data != p.exe {
		t.Errorf("DisplayIcon = %q", data)
	}
	if _, data := get("UninstallString"); data != `"`+p.exe+`" uninstall-service` {
		t.Errorf("UninstallString = %q", data)
	}
	for _, n := range []string{"NoModify", "NoRepair"} {
		if typ, data := get(n); typ != "REG_DWORD" || data != "1" {
			t.Errorf("%s = %s %q", n, typ, data)
		}
	}
	if got := regDeleteUninstallArgs(); len(got) != 3 || got[0] != "delete" || got[1] != uninstallKey || got[2] != "/f" {
		t.Errorf("regDeleteUninstallArgs = %v", got)
	}
}

func TestElevationCommand(t *testing.T) {
	self := `C:\Users\A B\Downloads\FileSync-Setup.exe`
	cmd := elevationCommand(self, "--elevated")
	for _, want := range []string{
		"Start-Process", "-FilePath '" + self + "'", "-Verb RunAs",
		"-Wait", "-ArgumentList '--elevated'",
		"try {", "catch { exit 1 }", "if ($null -eq $p) { exit 1 }",
		"exit $p.ExitCode",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("elevation command %q missing %q", cmd, want)
		}
	}
	// The exe path must never sit in a double-quoted PowerShell
	// string: $(...) and backtick escapes still execute there.
	if strings.Contains(cmd, `-FilePath "`) {
		t.Errorf("exe path double-quoted in elevation command: %q", cmd)
	}
}

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

func TestElevationCommandEvilPath(t *testing.T) {
	// A folder name on Windows may legally contain $, parens, a
	// backtick and an apostrophe. Inside a double-quoted string
	// the $(...) would execute in the parent shell before the
	// UAC prompt; psQuote must neutralise the whole path.
	evil := "C:\\Users\\$(calc) O'Neil\\`x\\FileSync-Setup.exe"
	cmd := elevationCommand(evil, "--elevated")
	if !strings.Contains(cmd, "-FilePath "+psQuote(evil)) {
		t.Errorf("evil path not psQuote-wrapped: %q", cmd)
	}
	out := psOutsideLiterals(cmd)
	for _, bad := range []string{"$(", "`", "calc", "FileSync-Setup"} {
		if strings.Contains(out, bad) {
			t.Errorf("payload fragment %q leaked outside string literals: %q (outside: %q)", bad, cmd, out)
		}
	}
}

const scQueryexStartPending = `
SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 2  START_PENDING 
                                (NOT_STOPPABLE, NOT_PAUSABLE, IGNORES_SHUTDOWN)
        WIN32_EXIT_CODE    : 0  (0x0)
        SERVICE_EXIT_CODE  : 0  (0x0)
        CHECKPOINT         : 0x1
        WAIT_HINT          : 0x7d0
        PID                : 6724
        FLAGS              :
`

const scQueryexStopped = `
SERVICE_NAME: FileSync 
        TYPE               : 10  WIN32_OWN_PROCESS  
        STATE              : 1  STOPPED 
        WIN32_EXIT_CODE    : 0  (0x0)
        SERVICE_EXIT_CODE  : 0  (0x0)
        CHECKPOINT         : 0x0
        WAIT_HINT          : 0x0
        PID                : 0
        FLAGS              :
`

func TestParseScQueryexPID(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"wedged service with process", scQueryexStartPending, 6724, true},
		{"stopped service has pid 0", scQueryexStopped, 0, true},
		{"not installed (1060)", scQueryNotInstalled, 0, false},
		{"query without ex has no pid line", scQueryRunning, 0, false},
		{"garbage", "blah blah\nnothing useful\n", 0, false},
		{"empty", "", 0, false},
		{"pid not a number", "        PID                : abc\n", 0, false},
		{"pid line without colon", "        PID                6724\n", 0, false},
		{"mid-line PID is not the field", "        FLAGS              : PID : 999\n", 0, false},
		{"decoy line before real pid", "        SOME_PID           : 999\n        PID                : 6724\n", 6724, true},
	}
	for _, c := range cases {
		got, ok := parseScQueryexPID(c.in)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: parseScQueryexPID = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
	if got := scQueryexArgs(); len(got) != 2 || got[0] != "queryex" || got[1] != "FileSync" {
		t.Errorf("scQueryexArgs = %v", got)
	}
}

func TestStuckAction(t *testing.T) {
	cases := []struct {
		state string
		want  stuckDecision
	}{
		{"STOPPED", stuckProceed},
		{"START_PENDING", stuckKillThenDelete},
		{"STOP_PENDING", stuckKillThenDelete},
		{"PAUSE_PENDING", stuckKillThenDelete},
		{"CONTINUE_PENDING", stuckKillThenDelete},
		{"RUNNING", stuckFail},
		{"PAUSED", stuckFail},
		{"NOT_INSTALLED", stuckFail},
		{"UNKNOWN", stuckFail},
		{"", stuckFail},
	}
	for _, c := range cases {
		if got := stuckAction(c.state); got != c.want {
			t.Errorf("stuckAction(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestTaskkillArgs(t *testing.T) {
	got := taskkillArgs(6724)
	want := []string{"/PID", "6724", "/F"}
	if len(got) != len(want) {
		t.Fatalf("taskkillArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("taskkillArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The setup-side copy of the QuickEdit bit math (D5): same
// contract as the internal/cli copy — 0x40 cleared, 0x80 set,
// every other bit preserved.
func TestConsoleModeWithoutQuickEditSetupCopy(t *testing.T) {
	cases := []struct{ in, want uint32 }{
		{0x01E7, 0x01A7},
		{0x0040, 0x0080},
		{0x0000, 0x0080},
		{0xFFFF, 0xFFFF &^ 0x0040},
	}
	for _, c := range cases {
		if got := consoleModeWithoutQuickEdit(c.in); got != c.want {
			t.Errorf("consoleModeWithoutQuickEdit(%#04x) = %#04x, want %#04x", c.in, got, c.want)
		}
	}
}

func TestShortcutCommand(t *testing.T) {
	p := computePaths(`C:\Users\O'Neil\AppData\Local`, `C:\Users\O'Neil\AppData\Roaming`)
	cmd := shortcutCommand(p)
	if !strings.Contains(cmd, "WScript.Shell") || !strings.Contains(cmd, "CreateShortcut") {
		t.Errorf("shortcut command missing WScript.Shell: %q", cmd)
	}
	if !strings.Contains(cmd, "$s.Arguments = 'ui'") {
		t.Errorf("shortcut must open the UI via `filesync.exe ui`: %q", cmd)
	}
	// Apostrophes in user names must be doubled for PowerShell
	// single-quoted literals.
	if !strings.Contains(cmd, `O''Neil`) {
		t.Errorf("apostrophe not escaped: %q", cmd)
	}
	if got := psQuote("a'b"); got != `'a''b'` {
		t.Errorf("psQuote = %q", got)
	}
}
