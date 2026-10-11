//go:build !unix && !windows && !darwin

package cli

import "errors"

// openBrowser fallback for platforms without a known opener: the
// CLI has already printed the URL for manual use.
func openBrowser(string) error {
	return errors.New("no browser opener on this platform")
}
