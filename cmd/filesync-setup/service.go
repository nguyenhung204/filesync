package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// Pure, platform-neutral helpers for the installer: they build the
// exact argv for the external tools (sc.exe, reg.exe, powershell)
// and parse their output. Keeping them free of side effects makes
// them unit-testable on Linux, where the real Windows flow cannot
// run. The command sequence mirrors deploy/windows/*.ps1 (job
// 005b): every sc.exe call is exit-code checked by the caller and
// success is only claimed after a RUNNING poll.

// ServiceName is the Windows service name; it must stay "FileSync"
// to match the deploy/windows scripts.
const ServiceName = "FileSync"

// uninstallKey is the per-user Add/Remove Programs registry key
// (HKCU because the install lives under %LOCALAPPDATA%).
const uninstallKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\FileSync`

// serviceDescription matches spec D3 step 5.
const serviceDescription = "Two-way file sync without a cloud (LAN)"

// paths collects every filesystem location the installer touches.
// Nothing outside installDir, configDir and shortcutPath is ever
// written, and configDir / the sync dir are never deleted (D4).
type paths struct {
	installDir   string
	exe          string
	configDir    string
	shortcutPath string
}

// computePaths derives the install locations from the per-user
// environment roots (%LOCALAPPDATA% and %APPDATA%).
func computePaths(localAppData, appData string) paths {
	installDir := filepath.Join(localAppData, "Programs", "filesync")
	return paths{
		installDir: installDir,
		exe:        filepath.Join(installDir, "filesync.exe"),
		configDir:  filepath.Join(appData, "filesync"),
		shortcutPath: filepath.Join(appData, "Microsoft", "Windows",
			"Start Menu", "Programs", "FileSync.lnk"),
	}
}

// binPathValue is the ImagePath value sc.exe stores: the quoted
// exe path followed by the serve arguments.
func binPathValue(exe, configDir string) string {
	return `"` + exe + `" serve --config-dir "` + configDir + `"`
}

// scQueryArgs returns argv for `sc.exe query FileSync`.
func scQueryArgs() []string { return []string{"query", ServiceName} }

// scStopArgs returns argv for `sc.exe stop FileSync`.
func scStopArgs() []string { return []string{"stop", ServiceName} }

// scDeleteArgs returns argv for `sc.exe delete FileSync`.
func scDeleteArgs() []string { return []string{"delete", ServiceName} }

// scStartArgs returns argv for `sc.exe start FileSync`.
func scStartArgs() []string { return []string{"start", ServiceName} }

// scCreateArgs returns argv for service creation. sc.exe wants
// "binPath=", "start=" and "DisplayName=" as separate tokens from
// their values, exactly like the PowerShell scripts pass them.
func scCreateArgs(exe, configDir string) []string {
	return []string{
		"create", ServiceName,
		"binPath=", binPathValue(exe, configDir),
		"start=", "auto",
		"DisplayName=", ServiceName,
	}
}

// scFailureArgs sets the restart-on-failure policy (matches the
// .ps1: reset counter after a day, restart after 5 seconds).
func scFailureArgs() []string {
	return []string{"failure", ServiceName, "reset=", "86400", "actions=", "restart/5000"}
}

// scDescriptionArgs sets the service description shown in
// services.msc.
func scDescriptionArgs() []string {
	return []string{"description", ServiceName, serviceDescription}
}

// parseScQueryState extracts the service state from `sc.exe query`
// output, e.g. the line "        STATE              : 4  RUNNING".
// It returns the state name ("RUNNING", "STOPPED", "START_PENDING",
// ...), "NOT_INSTALLED" when sc reports error 1060, or "UNKNOWN"
// for anything unparseable.
func parseScQueryState(output string) string {
	if strings.Contains(output, "1060") {
		return "NOT_INSTALLED"
	}
	for _, line := range strings.Split(output, "\n") {
		i := strings.Index(line, "STATE")
		if i < 0 {
			continue
		}
		rest := line[i+len("STATE"):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		fields := strings.Fields(rest[colon+1:])
		if len(fields) >= 2 {
			return fields[1]
		}
		if len(fields) == 1 {
			return fields[0]
		}
	}
	return "UNKNOWN"
}

// initInfo carries the fields the installer summary needs out of
// `filesync.exe init` output.
type initInfo struct {
	DeviceID    string
	Fingerprint string
	SyncDir     string
	ConfigDir   string
}

// parseInitOutput reads the "key:    value" lines that cmdInit
// always prints (both for a fresh init and for the idempotent
// "already initialized" rerun), so upgrades keep working.
func parseInitOutput(output string) initInfo {
	var info initInfo
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "device_id":
			info.DeviceID = value
		case "fingerprint":
			info.Fingerprint = value
		case "sync_dir":
			info.SyncDir = value
		case "config_dir":
			info.ConfigDir = value
		}
	}
	return info
}

// elevationCommand builds the PowerShell -Command payload that
// relaunches exePath elevated with the extra args appended. The
// parent waits for the child and exits with the child's exit code.
//
// exePath and the argument list are quoted with psQuote (single
// quotes). Inside a double-quoted PowerShell string, $(...)
// subexpressions and backtick escapes still execute, so an exe
// sitting in a folder whose name contains those characters would
// run attacker-chosen code in this still-unelevated parent shell
// before the UAC prompt even appears.
//
// If the user cancels or refuses the UAC prompt, Start-Process
// throws (or, defensively, yields no process): the payload exits
// 1 in both cases instead of dereferencing a null $p, which can
// exit 0 and make the parent close without a word.
func elevationCommand(exePath string, extraArgs ...string) string {
	argList := ""
	if len(extraArgs) > 0 {
		argList = " -ArgumentList " + psQuote(strings.Join(extraArgs, " "))
	}
	return "try { $p = Start-Process -FilePath " + psQuote(exePath) +
		" -Verb RunAs -Wait -PassThru" + argList +
		" } catch { exit 1 }; if ($null -eq $p) { exit 1 }; exit $p.ExitCode"
}

// psQuote quotes s as a PowerShell single-quoted string literal
// (embedded single quotes are doubled, per PowerShell rules).
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// shortcutCommand builds the PowerShell -Command payload that
// creates the Start Menu shortcut via WScript.Shell. The shortcut
// opens the Web UI through `filesync.exe ui`.
func shortcutCommand(p paths) string {
	return `$s = (New-Object -ComObject WScript.Shell).CreateShortcut(` + psQuote(p.shortcutPath) + `); ` +
		`$s.TargetPath = ` + psQuote(p.exe) + `; ` +
		`$s.Arguments = 'ui'; ` +
		`$s.WorkingDirectory = ` + psQuote(p.installDir) + `; ` +
		`$s.Description = 'Mo giao dien FileSync'; ` +
		`$s.Save()`
}

// regAddCommands returns one argv slice per registry value of the
// Add/Remove Programs entry; the caller runs each with reg.exe
// and checks every exit code.
func regAddCommands(p paths) [][]string {
	sz := func(name, data string) []string {
		return []string{"add", uninstallKey, "/v", name, "/t", "REG_SZ", "/d", data, "/f"}
	}
	dword := func(name, data string) []string {
		return []string{"add", uninstallKey, "/v", name, "/t", "REG_DWORD", "/d", data, "/f"}
	}
	return [][]string{
		sz("DisplayName", "FileSync"),
		sz("DisplayVersion", setupVersion),
		sz("InstallLocation", p.installDir),
		sz("DisplayIcon", p.exe),
		sz("UninstallString", `"`+p.exe+`" uninstall-service`),
		dword("NoModify", "1"),
		dword("NoRepair", "1"),
	}
}

// regDeleteUninstallArgs removes the whole Add/Remove Programs key
// (used by `filesync uninstall-service`).
func regDeleteUninstallArgs() []string {
	return []string{"delete", uninstallKey, "/f"}
}

// scQueryexArgs returns argv for `sc.exe queryex FileSync`: the
// query output plus the PID of the service's own process.
func scQueryexArgs() []string { return []string{"queryex", ServiceName} }

// parseScQueryexPID extracts the service process PID from
// `sc.exe queryex` output, e.g. the line
// "        PID                : 6724". ok is false when the
// output carries no parseable PID line (the 1060 error output,
// garbage). A PID of 0 parses with ok=true and means the
// service has no process.
func parseScQueryexPID(output string) (pid int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		i := strings.Index(line, "PID")
		if i < 0 {
			continue
		}
		rest := line[i+len("PID"):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		fields := strings.Fields(rest[colon+1:])
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return n, true
	}
	return 0, false
}

// taskkillArgs returns argv for force-killing exactly one
// process by PID: taskkill /PID <pid> /F.
func taskkillArgs(pid int) []string {
	return []string{"/PID", strconv.Itoa(pid), "/F"}
}

// stuckDecision is what to do when a pre-existing service did
// not reach STOPPED inside the stop timeout.
type stuckDecision int

const (
	stuckProceed stuckDecision = iota // already STOPPED: carry on to delete
	stuckKillThenDelete               // wedged *_PENDING: kill its process, then delete
	stuckFail                         // anything else: fail as before, hands off
)

// stuckAction decides from the last observed service state
// (job 008 spec D4). The kill branch is reserved for the
// *_PENDING family: a service whose process never speaks SCM
// wedges in one of those states forever and ignores stop —
// exactly the 0.2.0 START_PENDING bug found on a real machine.
func stuckAction(state string) stuckDecision {
	switch state {
	case "STOPPED":
		return stuckProceed
	case "START_PENDING", "STOP_PENDING", "PAUSE_PENDING", "CONTINUE_PENDING":
		return stuckKillThenDelete
	default:
		return stuckFail
	}
}

// consoleModeWithoutQuickEdit mirrors the helper in
// internal/cli (serve_scm.go): QuickEdit bit cleared,
// extended-flags bit set, every other bit preserved. It is a
// deliberate small copy next to this binary's own kernel32
// calls — package main cannot import internal/cli (the
// psQuote duplication precedent from job 007).
func consoleModeWithoutQuickEdit(mode uint32) uint32 {
	return (mode | 0x0080) &^ 0x0040
}
