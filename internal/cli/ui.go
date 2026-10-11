package cli

import (
	"fmt"
	"net"

	"filesync/internal/webui"
)

// cmdUI implements `filesync ui`: print the web UI URL (with the
// access token) and open it in the machine's browser. The token
// file only exists while a daemon with the UI enabled is (or was)
// running, so a missing token is reported, never created here.
func cmdUI(e *env, _ []string) int {
	cfg, err := e.loadConfig()
	if err != nil {
		return e.fail("ui: %v", err)
	}
	if !cfg.UIEnabled {
		return e.fail("ui: web UI is disabled (ui_enabled=false in config.json)")
	}
	token, err := webui.ReadToken(cfg.ConfigDir)
	if err != nil {
		return e.fail("ui: web UI token not found — is the daemon running?")
	}
	addr := ""
	if st, _, live := readLiveStatus(e); live && st.UIAddr != "" {
		addr = st.UIAddr
	}
	if addr == "" {
		addr = net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.EffectiveUIPort()))
	}
	url := "http://" + addr + "/?token=" + token
	fmt.Fprintln(e.stdout, url)
	fmt.Fprintln(e.stdout, "this URL contains your private UI token — do not share it")
	if err := openBrowser(url); err != nil {
		fmt.Fprintf(e.stdout, "could not open a browser automatically (%v) — copy the URL above into your browser\n", err)
	}
	return 0
}
