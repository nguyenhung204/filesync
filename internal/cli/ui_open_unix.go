//go:build unix && !darwin

package cli

import "os/exec"

// openBrowser opens url with the platform default handler. The
// opener is started detached: the CLI exits without waiting for
// the browser.
func openBrowser(url string) error {
	cmd := exec.Command("xdg-open", url)
	return cmd.Start()
}
