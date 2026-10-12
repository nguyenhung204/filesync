//go:build windows

package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// cmdUninstallService removes the FileSync Windows service, the
// Add/Remove Programs entry and the Start Menu shortcut. It is the
// target of the UninstallString that FileSync-Setup.exe registers
// ("<exe>" uninstall-service). It mirrors uninstall-service.ps1:
// config and synced data are NEVER deleted here.
//
// All output is plain ASCII (no diacritics), like the rest of the
// Windows install tooling (console codepage safety, job 005b).

const uninstallKeyPath = `HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\FileSync`

type svcResult struct {
	output string
	code   int
}

func svcRun(name string, args ...string) svcResult {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return svcResult{output: string(out), code: 0}
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return svcResult{output: string(out), code: exitErr.ExitCode()}
	}
	return svcResult{output: string(out), code: -1}
}

func svcIsAdmin() bool {
	return svcRun("net", "session").code == 0
}

func svcHasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// svcWaitEnter holds the console open: Windows Settings launches
// the UninstallString in its own window which would otherwise
// close before the user can read the result.
func svcWaitEnter(e *env) {
	fmt.Fprint(e.stdout, "\nNhan Enter de dong cua so nay...")
	_, _ = bufio.NewReader(e.stdin).ReadString('\n')
}

func cmdUninstallService(e *env, args []string) int {
	if !svcIsAdmin() {
		if svcHasArg(args, "--elevated") {
			fmt.Fprintln(e.stdout, "[LOI] Da nang quyen nhung van chua co quyen admin.")
			fmt.Fprintln(e.stdout, "Hay chuot phai vao filesync.exe va chon \"Run as administrator\", roi chay lai lenh uninstall-service.")
			svcWaitEnter(e)
			return 1
		}
		return svcRelayElevated(e)
	}
	code := svcDoUninstall(e)
	svcWaitEnter(e)
	return code
}

// svcRelayElevated relaunches filesync.exe elevated with the
// uninstall arguments and propagates the child's exit code.
func svcRelayElevated(e *env) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(e.stdout, "[LOI] Khong xac dinh duoc duong dan filesync.exe: %v\n", err)
		svcWaitEnter(e)
		return 1
	}
	res := svcRun("powershell", "-NoProfile", "-Command", svcElevationPayload(self))
	if res.code != 0 {
		fmt.Fprintln(e.stdout, "[LOI] Khong nang quyen duoc (hop thoai bi huy hoac bi tu choi).")
		fmt.Fprintln(e.stdout, "Hay mo PowerShell quyen admin va chay: filesync.exe uninstall-service")
		svcWaitEnter(e)
		return 1
	}
	return res.code
}

// svcDoUninstall performs the removal as administrator.
func svcDoUninstall(e *env) int {
	if svcRun("sc.exe", "query", "FileSync").code != 0 {
		fmt.Fprintln(e.stdout, "FileSync service chua duoc cai tren may nay.")
		return 0
	}
	// Stop first; ignore its exit code (it errors when the
	// service is not running) — the STOPPED poll below is the
	// real gate, exactly like uninstall-service.ps1.
	_ = svcRun("sc.exe", "stop", "FileSync")
	state, stopped := svcPollState("STOPPED", 15*time.Second)
	if !stopped {
		if svcStuckAction(state) != svcStuckKillThenDelete {
			fmt.Fprintf(e.stdout, "[LOI] Service chua dung han sau 15 giay (trang thai cuoi: %s).\n", state)
			return 1
		}
		// The 0.2.0 bug (spec D4): a service whose process
		// never speaks SCM wedges in a *_PENDING state forever
		// and ignores stop. Kill exactly the process queryex
		// attributes to THIS service — never any other PID.
		if !svcKillServiceProcess(e, state) || !svcWaitStoppedOrGone(5*time.Second) {
			fmt.Fprintln(e.stdout, "[LOI] Khong tat duoc tien trinh cua service dang treo.")
			return 1
		}
	}
	res := svcRun("sc.exe", "delete", "FileSync")
	if res.code != 0 {
		fmt.Fprintf(e.stdout, "[LOI] sc.exe delete tra ma %d: %s\n", res.code, res.output)
		return 1
	}
	if !svcPollGone(15 * time.Second) {
		fmt.Fprintln(e.stdout, "[LOI] Service van con sau 15 giay ke tu khi xoa.")
		return 1
	}
	if res := svcRun("reg.exe", "delete", uninstallKeyPath, "/f"); res.code != 0 {
		fmt.Fprintf(e.stdout, "[CANH BAO] Khong xoa duoc muc go cai dat trong Registry (reg delete tra ma %d).\n", res.code)
	}
	if appData := os.Getenv("APPDATA"); appData != "" {
		shortcut := filepath.Join(appData, "Microsoft", "Windows",
			"Start Menu", "Programs", "FileSync.lnk")
		_ = os.Remove(shortcut)
	}
	// Desktop shortcut (job 009): the Desktop folder may be
	// redirected, so removal goes through the PowerShell idiom
	// the installer used to create it. Best-effort throughout.
	_ = svcRun("powershell", "-NoProfile", "-Command", svcDesktopShortcutRemoveCommand())
	// Firewall rules (job 009 addendum): remove what the
	// installer added. Best-effort — a rule the user already
	// deleted by hand is not an error worth reporting.
	for _, args := range svcFirewallDeleteCommands() {
		_ = svcRun("netsh", args...)
	}
	installDir := ""
	if self, err := os.Executable(); err == nil {
		installDir = filepath.Dir(self)
	}
	fmt.Fprintln(e.stdout, "Da go service FileSync.")
	fmt.Fprintln(e.stdout, "Config va du lieu dong bo KHONG bi xoa.")
	fmt.Fprintf(e.stdout, "Muon xoa han, hay xoa thu muc cai dat %s va thu muc cau hinh %s.\n",
		installDir, e.configDir)
	return 0
}

// svcPollState polls `sc query` every 500ms until the state
// equals want or the timeout passes; it returns the last seen
// state.
func svcPollState(want string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	last := "UNKNOWN"
	for {
		res := svcRun("sc.exe", "query", svcServiceName)
		last = svcParseScQueryState(res.output)
		if last == want {
			return last, true
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// svcKillServiceProcess force-kills the process that
// `sc.exe queryex` attributes to the FileSync service and
// reports whether taskkill succeeded. A missing or zero PID
// means there is no process to kill — a failure here, since
// the caller only invokes this for a wedged *_PENDING service.
func svcKillServiceProcess(e *env, state string) bool {
	res := svcRun("sc.exe", "queryex", svcServiceName)
	pid, ok := svcParseScQueryexPID(res.output)
	if !ok || pid <= 0 {
		return false
	}
	fmt.Fprintf(e.stdout, "Service dang treo o trang thai %s; dang tat tien trinh cua service (PID %d)...\n", state, pid)
	return svcRun("taskkill", svcTaskkillArgs(pid)...).code == 0
}

// svcWaitStoppedOrGone polls until the service reports
// STOPPED or disappears entirely (after a kill the SCM entry
// can vanish together with the process).
func svcWaitStoppedOrGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		res := svcRun("sc.exe", "query", svcServiceName)
		if res.code != 0 || svcParseScQueryState(res.output) == "STOPPED" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// svcPollGone polls `sc query` until the service is gone (exit
// code 1060) or the timeout passes.
func svcPollGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if svcRun("sc.exe", "query", "FileSync").code != 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}
