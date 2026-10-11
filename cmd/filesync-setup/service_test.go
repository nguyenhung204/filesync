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
		"Start-Process", `-FilePath "` + self + `"`, "-Verb RunAs",
		"-Wait", `-ArgumentList "--elevated"`, "exit $p.ExitCode",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("elevation command %q missing %q", cmd, want)
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
