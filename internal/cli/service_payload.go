package cli

import (
	"strconv"
	"strings"
)

// This file builds the PowerShell payload used to relaunch
// filesync.exe elevated for `uninstall-service`. It is kept
// platform-neutral (no build tag) so the payload shape is
// unit-testable on Linux, the same way cmd/filesync-setup keeps
// its helpers in a neutral service.go.

// svcPsQuote quotes s as a PowerShell single-quoted string
// literal (embedded single quotes are doubled). It duplicates
// psQuote in cmd/filesync-setup on purpose: that helper lives in
// a package main and cannot be imported here, and a path must
// never be interpolated into a double-quoted PowerShell string,
// where $(...) subexpressions and backtick escapes still run.
func svcPsQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// svcElevationPayload relaunches exePath elevated with the
// uninstall arguments, waits for the child and exits with the
// child's exit code. The path and the argument list are quoted
// with svcPsQuote: inside a double-quoted PowerShell string,
// $(...) subexpressions and backtick escapes still execute, so a
// hostile folder name would otherwise run code in this
// still-unelevated parent shell before the UAC prompt appears.
// If the user cancels or refuses UAC, Start-Process throws (or,
// defensively, yields no process): the payload exits 1 in both
// cases instead of dereferencing a null $p, which can exit 0 and
// make the parent close without a word.
func svcElevationPayload(exePath string) string {
	return "try { $p = Start-Process -FilePath " + svcPsQuote(exePath) +
		" -Verb RunAs -Wait -PassThru -ArgumentList " + svcPsQuote("uninstall-service --elevated") +
		" } catch { exit 1 }; if ($null -eq $p) { exit 1 }; exit $p.ExitCode"
}

// The helpers below support the stuck-service fallback of job
// 008 spec D4 in `filesync uninstall-service`. They mirror the
// helpers of the same names in cmd/filesync-setup/service.go —
// deliberate copies again (package main cannot be imported),
// kept behaviour-identical on purpose.

const svcServiceName = "FileSync"

// svcParseScQueryState extracts the service state name from
// `sc.exe query`/`queryex` output ("RUNNING", "STOPPED",
// "START_PENDING", ...), "NOT_INSTALLED" on the 1060 error
// output, or "UNKNOWN" for anything unparseable.
func svcParseScQueryState(output string) string {
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

// svcParseScQueryexPID extracts the service process PID from
// `sc.exe queryex` output. ok is false when no parseable PID
// line exists; PID 0 parses with ok=true and means the service
// has no process.
func svcParseScQueryexPID(output string) (pid int, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		// Anchored at the line start (after indent): a stray
		// "PID" inside another line's text must never be read
		// as the service PID (job 008 approver NIT 2).
		trimmed := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trimmed, "PID") {
			continue
		}
		rest := trimmed[len("PID"):]
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

// svcDesktopShortcutRemoveCommand builds the PowerShell
// -Command payload that removes the Desktop shortcut FileSync
// (job 009 spec D1). The Desktop folder is resolved by
// PowerShell ([Environment]::GetFolderPath) because it may be
// redirected (OneDrive) — the same idiom the installer uses to
// create it. Removal is best-effort by design (SilentlyContinue);
// there is no path interpolation at all in this payload.
func svcDesktopShortcutRemoveCommand() string {
	return `$desk = [Environment]::GetFolderPath('Desktop'); ` +
		`if ($desk) { Remove-Item (Join-Path $desk 'FileSync.lnk') -Force -ErrorAction SilentlyContinue }`
}

// The FileSync firewall rule names (job 009 addendum). They
// mirror firewallRuleTCP/UDP in cmd/filesync-setup/service.go —
// deliberate copies again (package main cannot be imported),
// kept identical on purpose so install/uninstall stay symmetric.
const (
	svcFirewallRuleTCP = "FileSync (TCP 22000)"
	svcFirewallRuleUDP = "FileSync (UDP 5353)"
)

// svcFirewallDeleteCommands returns netsh argv pairs removing
// both FileSync inbound rules at uninstall.
func svcFirewallDeleteCommands() [][]string {
	del := func(name string) []string {
		return []string{"advfirewall", "firewall", "delete", "rule", "name=" + name}
	}
	return [][]string{del(svcFirewallRuleTCP), del(svcFirewallRuleUDP)}
}

// svcTaskkillArgs returns argv for force-killing exactly one
// process by PID: taskkill /PID <pid> /F.
func svcTaskkillArgs(pid int) []string {
	return []string{"/PID", strconv.Itoa(pid), "/F"}
}

// svcStuckDecision is what to do when the service did not
// reach STOPPED inside the stop timeout.
type svcStuckDecision int

const (
	svcStuckProceed        svcStuckDecision = iota // already STOPPED: carry on
	svcStuckKillThenDelete                         // wedged *_PENDING: kill its process, then delete
	svcStuckFail                                   // anything else: fail, hands off
)

// svcStuckAction decides from the last observed service
// state. The kill branch is reserved for the *_PENDING
// family: a service whose process never speaks SCM wedges in
// one of those states forever and ignores stop — the 0.2.0
// START_PENDING bug.
func svcStuckAction(state string) svcStuckDecision {
	switch state {
	case "STOPPED":
		return svcStuckProceed
	case "START_PENDING", "STOP_PENDING", "PAUSE_PENDING", "CONTINUE_PENDING":
		return svcStuckKillThenDelete
	default:
		return svcStuckFail
	}
}
