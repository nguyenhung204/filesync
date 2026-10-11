// Command filesync is the FileSync daemon + CLI (phase 1: LAN
// two-way file sync without a cloud intermediary).
package main

import (
	"os"

	"filesync/internal/cli"
)

// version is overridable at build time via -ldflags -X main.version.
var version = "0.1.0"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
