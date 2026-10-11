package cli

import "strings"

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
