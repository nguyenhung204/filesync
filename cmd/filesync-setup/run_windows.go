//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// The Windows install flow (spec D3). Every external command goes
// through runCmd and its exit code is checked on the spot; the
// word "xong" (done) is only printed after the RUNNING gate.

// cmdResult is one finished external command.
type cmdResult struct {
	output string
	code   int
}

// runCmd runs name with args, capturing combined output, and
// reports the process exit code (-1 when it could not start).
func runCmd(name string, args ...string) cmdResult {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return cmdResult{output: string(out), code: 0}
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return cmdResult{output: string(out), code: exitErr.ExitCode()}
	}
	return cmdResult{output: string(out), code: -1}
}

// isAdmin probes for elevation the way the .bat flow effectively
// does: `net session` only succeeds for administrators.
func isAdmin() bool {
	return runCmd("net", "session").code == 0
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// waitEnter holds the console window open so a double-click user
// can read the outcome before it closes.
func waitEnter() {
	fmt.Print("\nNhan Enter de dong cua so nay...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// fail prints a [LOI] step failure with detail and returns 1.
func fail(step, detail string) int {
	fmt.Printf("[LOI] %s: %s\n", step, detail)
	return 1
}

var (
	kernel32DLL        = syscall.NewLazyDLL("kernel32.dll")
	procGetStdHandle   = kernel32DLL.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32DLL.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32DLL.NewProc("SetConsoleMode")
)

// disableConsoleQuickEdit turns off console QuickEdit before
// anything else runs (spec D5): in QuickEdit "Select" state a
// click into the window freezes the process, which hung this
// very installer on a real machine. Best effort: any failure
// (no console attached, redirected stdio) is silent.
func disableConsoleQuickEdit() {
	// STD_INPUT_HANDLE is -10; written as a complement because
	// a negative constant cannot convert to uintptr.
	handle, _, _ := procGetStdHandle.Call(^uintptr(9))
	if handle == 0 || handle == ^uintptr(0) {
		return
	}
	var mode uint32
	r1, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode)))
	if r1 == 0 {
		return
	}
	r1, _, _ = procSetConsoleMode.Call(handle, uintptr(consoleModeWithoutQuickEdit(mode)))
	_ = r1 // best effort: a failed mode set must not block the install
}

// run is the installer entry point.
func run() int {
	disableConsoleQuickEdit()
	args := os.Args[1:]
	if !isAdmin() {
		if hasArg(args, "--elevated") {
			// Already relaunched once and still not admin: do
			// not loop elevation forever.
			code := fail("Quyen admin",
				"da nang quyen nhung van chua co quyen admin")
			fmt.Println("Hay chuot phai vao FileSync-Setup.exe va chon \"Run as administrator\".")
			waitEnter()
			return code
		}
		code := relayElevated()
		// The non-elevated parent is the process that opens the
		// UI after a successful install (spec D1/F1): it runs as
		// the real user, so the browser lands in the right
		// session. The elevated child never opens it.
		if shouldOpenUI(false, args, code) {
			if p, ok := currentUserPaths(); ok {
				openUIAfterInstall(p)
			}
		}
		return code
	}
	code := install()
	if shouldOpenUI(true, args, code) {
		// Direct "Run as administrator" (no relay, no sentinel):
		// this same process opens the UI on success. With the
		// sentinel present shouldOpenUI is false — the parent
		// handles it (spec D1).
		if p, ok := currentUserPaths(); ok {
			openUIAfterInstall(p)
		}
	}
	waitEnter()
	return code
}

// currentUserPaths derives the install paths from THIS
// process's environment (the user's own profile variables).
func currentUserPaths() (paths, bool) {
	localAppData := os.Getenv("LOCALAPPDATA")
	appData := os.Getenv("APPDATA")
	if localAppData == "" || appData == "" {
		return paths{}, false
	}
	return computePaths(localAppData, appData), true
}

// openUIAfterInstall waits (briefly) for the freshly started
// service's Web UI to publish its token file, then launches
// `filesync.exe ui`, which opens the browser already signed in.
// Entirely best-effort (spec D1): any failure only prints the
// fallback line, and the install's exit code never changes.
func openUIAfterInstall(p paths) {
	tokenPath := filepath.Join(p.configDir, "ui-token")
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(tokenPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("Mo giao dien bang shortcut FileSync tren Desktop hoac Start Menu.")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := exec.Command(p.exe, "ui").Start(); err != nil {
		fmt.Printf("[CANH BAO] Khong tu mo duoc giao dien: %v\n", err)
		fmt.Println("Mo giao dien bang shortcut FileSync tren Desktop hoac Start Menu.")
		return
	}
	fmt.Println("Da mo giao dien FileSync trong trinh duyet.")
}

// relayElevated relaunches this exe elevated and waits for the
// child, propagating its exit code. This parent window does not
// wait for Enter: the elevated child owns a fresh console that
// stays open at the end of its run.
func relayElevated() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Printf("[LOI] Khong xac dinh duoc duong dan cua chinh file cai dat: %v\n", err)
		waitEnter()
		return 1
	}
	fmt.Println("FileSync can quyen admin de dang ky Windows service.")
	fmt.Println("Dang mo hop thoai xin quyen... chon Yes de tiep tuc.")
	res := runCmd("powershell", "-NoProfile", "-Command",
		elevationCommand(self, "--elevated"))
	if res.code != 0 {
		fmt.Println("[LOI] Khong nang quyen duoc (hop thoai bi huy hoac bi tu choi).")
		fmt.Println("Hay chuot phai vao FileSync-Setup.exe va chon \"Run as administrator\".")
		waitEnter()
		return 1
	}
	return res.code
}

// install runs steps 2-8 of spec D3 as phases [1/6]..[6/6].
func install() int {
	localAppData := os.Getenv("LOCALAPPDATA")
	appData := os.Getenv("APPDATA")
	if localAppData == "" || appData == "" {
		return fail("Moi truong", "khong doc duoc LOCALAPPDATA/APPDATA")
	}
	p := computePaths(localAppData, appData)

	fmt.Println("[1/6] Kiem tra service cu...")
	if code := removeOldService(); code != 0 {
		return code
	}

	fmt.Println("[2/6] Cai dat filesync.exe...")
	if code := extractPayload(p); code != 0 {
		return code
	}

	fmt.Println("[3/6] Khoi tao cau hinh va dinh danh may...")
	info, code := runInit(p)
	if code != 0 {
		return code
	}

	fmt.Println("[4/6] Dang ky Windows service...")
	if code := registerService(p); code != 0 {
		return code
	}

	fmt.Println("[5/6] Kiem chung service dang chay...")
	if code := verifyRunning(); code != 0 {
		return code
	}

	fmt.Println("[6/6] Tao shortcut Desktop + Start Menu, icon va muc go cai dat...")
	warnShortcutAndRegistry(p)

	printSummary(p, info)
	return 0
}

// removeOldService stops and deletes a pre-existing FileSync
// service (the upgrade path, spec D4). Absence is not an error.
func removeOldService() int {
	if runCmd("sc.exe", scQueryArgs()...).code != 0 {
		return 0 // not installed: nothing to do
	}
	// Ignore stop's own exit code: it errors when the service is
	// already stopped; the STOPPED poll below is the real gate.
	_ = runCmd("sc.exe", scStopArgs()...)
	state, stopped := pollState("STOPPED", 15*time.Second)
	if !stopped {
		if stuckAction(state) != stuckKillThenDelete {
			return fail("Dung service cu",
				"service cu chua dung han sau 15 giay (trang thai cuoi: "+state+")")
		}
		// The 0.2.0 bug (spec D4): a service whose process never
		// speaks SCM wedges in a *_PENDING state forever and
		// ignores stop, so a plain timeout would fail every
		// upgrade. Kill exactly the process queryex attributes
		// to THIS service — never any other PID — then proceed.
		if !killServiceProcess(state) {
			return fail("Dung service cu",
				"service cu dang treo o trang thai "+state+" va khong tat duoc tien trinh cua no")
		}
		if !waitStoppedOrGone(5 * time.Second) {
			return fail("Dung service cu", "tien trinh service cu van chua dung sau khi bi tat")
		}
	}
	res := runCmd("sc.exe", scDeleteArgs()...)
	if res.code != 0 {
		return fail("Xoa service cu",
			fmt.Sprintf("sc.exe delete tra ma %d: %s", res.code, strings.TrimSpace(res.output)))
	}
	if !pollGone(15 * time.Second) {
		return fail("Xoa service cu", "service cu van con sau 15 giay")
	}
	return 0
}

// extractPayload writes the embedded daemon binary to the install
// dir via a sibling temp file + rename, so a crash never leaves a
// half-written exe behind.
func extractPayload(p paths) int {
	if len(payload) < 2 || payload[0] != 'M' || payload[1] != 'Z' || len(payload) < 1<<20 {
		return fail("Cai dat",
			"ban build nay chua nhung filesync.exe that (dang la placeholder) - hay tai ban Release chinh thuc")
	}
	if err := os.MkdirAll(p.installDir, 0o755); err != nil {
		return fail("Cai dat", "khong tao duoc thu muc "+p.installDir+": "+err.Error())
	}
	tmp := filepath.Join(p.installDir, "filesync.exe.tmp")
	if err := os.WriteFile(tmp, payload, 0o755); err != nil {
		return fail("Cai dat", "khong ghi duoc file tam: "+err.Error())
	}
	if err := os.Rename(tmp, p.exe); err != nil {
		// Windows refuses to rename over a locked exe; the old
		// service is stopped by now, so remove and retry once.
		_ = os.Remove(p.exe)
		if err2 := os.Rename(tmp, p.exe); err2 != nil {
			_ = os.Remove(tmp)
			return fail("Cai dat", "khong thay duoc filesync.exe: "+err2.Error())
		}
	}
	// The brand icon rides along next to the exe (spec D1): the
	// shortcuts and the Add/Remove Programs entry point at it. A
	// failed icon write only warns — the install itself is done.
	if len(icon) < 4 || string(icon[:4]) != "\x00\x00\x01\x00" {
		fmt.Println("[CANH BAO] Ban build nay thieu file icon FileSync (filesync.ico).")
	} else if err := os.WriteFile(p.iconPath, icon, 0o644); err != nil {
		fmt.Printf("[CANH BAO] Khong ghi duoc file icon: %v\n", err)
	}
	return 0
}

// runInit runs `filesync.exe init` (idempotent; safe on upgrades)
// and parses the device identity it prints.
func runInit(p paths) (initInfo, int) {
	res := runCmd(p.exe, "init")
	if res.code != 0 {
		return initInfo{}, fail("Khoi tao",
			fmt.Sprintf("filesync.exe init tra ma %d: %s", res.code, strings.TrimSpace(res.output)))
	}
	fmt.Print(res.output)
	info := parseInitOutput(res.output)
	if info.DeviceID == "" {
		return initInfo{}, fail("Khoi tao", "khong doc duoc device_id tu output cua init")
	}
	return info, 0
}

// registerService creates the service and sets its failure policy
// and description, checking the exit code after every sc.exe call
// (the 005b lesson: never swallow an sc error).
func registerService(p paths) int {
	steps := []struct {
		name string
		args []string
	}{
		{"create", scCreateArgs(p.exe, p.configDir)},
		{"failure", scFailureArgs()},
		{"description", scDescriptionArgs()},
		{"start", scStartArgs()},
	}
	for _, s := range steps {
		res := runCmd("sc.exe", s.args...)
		if res.code != 0 {
			return fail("Dang ky service",
				fmt.Sprintf("sc.exe %s tra ma %d: %s", s.name, res.code, strings.TrimSpace(res.output)))
		}
	}
	return 0
}

// verifyRunning is the hard gate of spec D3 step 6: poll the
// service state until RUNNING; on timeout report the last state.
func verifyRunning() int {
	state, ok := pollState("RUNNING", 15*time.Second)
	if !ok {
		fmt.Printf("[LOI] Kiem chung: service chua chay sau 15 giay (trang thai cuoi: %s).\n", state)
		fmt.Println("Mo PowerShell va chay lenh sau de xem log cua daemon:")
		fmt.Println("  filesync.exe serve   (trong thu muc cai dat)")
		return 1
	}
	return 0
}

// pollState polls `sc query` every 500ms until the state equals
// want or the timeout passes; it returns the last seen state.
func pollState(want string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	last := "UNKNOWN"
	for {
		res := runCmd("sc.exe", scQueryArgs()...)
		last = parseScQueryState(res.output)
		if last == want {
			return last, true
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// killServiceProcess force-kills the process that
// `sc.exe queryex` attributes to the FileSync service and
// reports whether taskkill succeeded. A missing or zero PID
// means there is no process to kill, which is a failure here:
// the caller only invokes this for a wedged *_PENDING service.
func killServiceProcess(state string) bool {
	res := runCmd("sc.exe", scQueryexArgs()...)
	pid, ok := parseScQueryexPID(res.output)
	if !ok || pid <= 0 {
		return false
	}
	fmt.Printf("Service cu dang treo o trang thai %s; dang tat tien trinh cua service (PID %d)...\n", state, pid)
	return runCmd("taskkill", taskkillArgs(pid)...).code == 0
}

// waitStoppedOrGone polls until the service reports STOPPED
// or disappears entirely (after a kill the SCM entry can
// vanish together with the process).
func waitStoppedOrGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		res := runCmd("sc.exe", scQueryArgs()...)
		if res.code != 0 || parseScQueryState(res.output) == "STOPPED" {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// pollGone polls until `sc query` reports the service is gone.
func pollGone(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if runCmd("sc.exe", scQueryArgs()...).code != 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// warnShortcutAndRegistry performs spec D3 step 7 plus the job
// 009 additions (Desktop shortcut via the same payload, brand
// icon, firewall rules). Failures here only warn: the service
// is already installed and RUNNING, so the install must not be
// rolled back for a missing shortcut or a refused firewall rule.
func warnShortcutAndRegistry(p paths) {
	res := runCmd("powershell", "-NoProfile", "-Command", shortcutCommand(p))
	if res.code != 0 {
		fmt.Printf("[CANH BAO] Khong tao duoc shortcut Desktop/Start Menu (powershell tra ma %d).\n", res.code)
		fmt.Println("Ban van mo giao dien bang lenh: filesync.exe ui")
	}
	warnFirewallRules(p)
	for _, args := range regAddCommands(p) {
		res := runCmd("reg.exe", args...)
		if res.code != 0 {
			fmt.Printf("[CANH BAO] Khong ghi duoc muc go cai dat (reg add %s tra ma %d).\n",
				args[3], res.code)
			fmt.Println("Service van chay binh thuong; go cai dat bang uninstall-service.ps1 hoac lenh filesync.exe uninstall-service.")
			return
		}
	}
}

// warnFirewallRules (re)creates the FileSync inbound firewall
// rules (job 009 addendum): without them Windows Firewall can
// silently block sync (TCP) and discovery (UDP 5353), above all
// for the service. Delete-then-add keeps reruns idempotent.
// Any failure warns with the manual route and never blocks the
// install.
func warnFirewallRules(p paths) {
	for _, args := range firewallDeleteCommands() {
		_ = runCmd("netsh", args...) // absent rule = fine here
	}
	for _, args := range firewallAddCommands(p.exe) {
		res := runCmd("netsh", args...)
		if res.code != 0 {
			fmt.Printf("[CANH BAO] Khong mo duoc tuong lua cho FileSync (%s, netsh tra ma %d).\n",
				args[4], res.code)
			fmt.Println("Neu may kia khong thay may nay, hay mo Windows Firewall va cho phep filesync.exe o mang Private.")
			return
		}
	}
}

// printSummary closes a successful install (spec D3 step 8).
func printSummary(p paths, info initInfo) {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("Cai dat xong. FileSync se tu chay moi khi mo may.")
	fmt.Println("Device ID:   " + info.DeviceID)
	fmt.Println("Fingerprint: " + info.Fingerprint)
	if info.SyncDir != "" {
		fmt.Println("Thu muc dong bo: " + info.SyncDir)
	}
	fmt.Println("Mo giao dien quan ly: shortcut FileSync tren Desktop hoac Start Menu (hoac chay filesync.exe ui)")
	fmt.Println("File exe nam tai: " + p.exe)
	fmt.Println("============================================================")
}
