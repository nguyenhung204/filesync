//go:build windows

package cli

import "os/exec"

// openBrowser opens url with the platform default handler. The
// opener is started detached: the CLI exits without waiting for
// the browser.
func openBrowser(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	return cmd.Start()
}
