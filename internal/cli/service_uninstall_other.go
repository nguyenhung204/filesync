//go:build !windows

package cli

import "fmt"

// cmdUninstallService only exists to serve the Windows
// Add/Remove Programs entry; everywhere else it fails loudly
// (pattern of the ui_open_* stubs).
func cmdUninstallService(e *env, _ []string) int {
	fmt.Fprintln(e.stdout, "uninstall-service chi ho tro tren Windows.")
	return 1
}
