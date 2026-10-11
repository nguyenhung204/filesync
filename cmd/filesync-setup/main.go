// Command filesync-setup is the one-file Windows installer for
// FileSync: double-click it and it elevates, installs the embedded
// filesync.exe, registers the service, and only reports success
// after the service is verified RUNNING. All console output is
// plain ASCII (no diacritics) because the Windows console codepage
// is not guaranteed to be UTF-8 (see job 005b encoding lesson).
package main

import "os"

// setupVersion must match the daemon version in cmd/filesync
// (var version). scripts/build-release.sh greps both and fails
// the packaging if they drift.
const setupVersion = "0.2.0"

func main() {
	os.Exit(run())
}
