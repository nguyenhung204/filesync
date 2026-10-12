// Command filesync-setup is the one-file Windows installer for
// FileSync: double-click it and it elevates, installs the embedded
// filesync.exe, registers the service, and only reports success
// after the service is verified RUNNING. Console output is full
// Vietnamese (with diacritics): run() switches the console to
// UTF-8 (code page 65001) before printing anything — the job
// 005b lesson was that text must match the console codepage,
// and setting the codepage is the half that makes dấu safe.
package main

import "os"

// setupVersion must match the daemon version in cmd/filesync
// (var version). scripts/build-release.sh greps both and fails
// the packaging if they drift.
const setupVersion = "0.3.0"

func main() {
	os.Exit(run())
}
