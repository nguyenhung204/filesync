//go:build windows

package cli

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"filesync/internal/daemon"
	"filesync/internal/logx"
)

// Windows service (SCM) integration for `filesync serve`
// (job 008 spec D2/D3/D5). The daemon binary is registered as
// the "FileSync" service by the installer with the plain
// command line `filesync.exe serve --config-dir <dir>`, so the
// entry point here is dispatcher-first: try to connect to the
// Service Control Manager, and only when the SCM did not start
// this process (error 1063) fall back to the console behaviour.
// No --service flag exists: an operator typing one at a console
// would hang ~30s waiting for an SCM that never comes.
//
// Everything goes through stdlib syscall.NewLazyDLL (advapi32
// + kernel32): golang.org/x/sys is only an indirect dependency
// and the repo keeps go.mod unchanged for service work. Every
// Proc.Call result is checked on the spot.

// serviceName must match the name the installer registers
// (cmd/filesync-setup ServiceName and the deploy scripts).
const serviceName = "FileSync"

// SCM constants (winsvc.h / winerror.h).
const (
	serviceWin32OwnProcess = 0x00000010

	stateStopped      = 1
	stateStartPending = 2
	stateStopPending  = 3
	stateRunning      = 4

	acceptStop     = 0x00000001
	acceptShutdown = 0x00000004

	ctrlStop        = 1
	ctrlInterrogate = 4
	ctrlShutdown    = 5

	errCallNotImplemented = 120

	// stdInputHandle is STD_INPUT_HANDLE (-10) as an unsigned
	// bit pattern; a negative constant cannot convert to
	// uintptr, hence the complement form.
	stdInputHandle   = ^uintptr(9)
	startWaitHintMs  = 30_000 // initial scan on a slow disk can take a while
	stopWaitHintMs   = 10_000
	invalidHandleVal = ^uintptr(0)
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
	procGetStdHandle                  = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode                = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode                = kernel32.NewProc("SetConsoleMode")
)

// serviceStatus mirrors the SERVICE_STATUS struct (7 DWORDs).
type serviceStatus struct {
	serviceType             uint32
	currentState            uint32
	controlsAccepted        uint32
	win32ExitCode           uint32
	serviceSpecificExitCode uint32
	checkPoint              uint32
	waitHint                uint32
}

// serviceTableEntry mirrors SERVICE_TABLE_ENTRYW: a service
// name pointer and its ServiceMain callback, in a slice closed
// by a {nil, 0} entry.
type serviceTableEntry struct {
	name *uint16
	proc uintptr
}

// serviceHost carries the state shared between the dispatcher
// thread (ServiceMain) and the control handler callbacks.
// Exactly one service runs per process, so a single global
// host is installed before the dispatcher starts.
type serviceHost struct {
	d   *daemon.Daemon
	log *logx.Logger

	mu     sync.Mutex
	handle uintptr // SERVICE_STATUS_HANDLE, 0 until registered
	state  uint32
	cancel context.CancelFunc
	runErr error // daemon result, read after the dispatcher returns
}

var activeServiceHost *serviceHost

// Callbacks are created once per process and kept referenced
// here so the GC cannot collect them while the SCM holds the
// raw pointers.
var (
	svcMainCallback    uintptr
	svcHandlerCallback uintptr
)

// runServe tries the SCM dispatcher first (D2). In service
// mode it blocks until the service ends and reports the
// daemon's result; error 1063 means a human started the
// process in a console, so it disables QuickEdit (D5) and
// runs the historical foreground path.
func runServe(e *env, d *daemon.Daemon, log *logx.Logger) int {
	host := &serviceHost{d: d, log: log, state: stateStopped}
	activeServiceHost = host
	svcMainCallback = syscall.NewCallback(host.serviceMain)
	svcHandlerCallback = syscall.NewCallback(serviceCtrlHandler)

	namePtr, err := syscall.UTF16PtrFromString(serviceName)
	if err != nil {
		return e.fail("serve: service name: %v", err)
	}
	table := []serviceTableEntry{
		{name: namePtr, proc: svcMainCallback},
		{name: nil, proc: 0},
	}
	r1, _, e1 := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r1 == 0 {
		errno := callErrno(e1)
		if serveModeForDispatch(errno) == serveModeConsole {
			disableConsoleQuickEdit()
			return runConsoleServe(e, d)
		}
		return e.fail("serve: connect to service control manager failed (error %d)", errno)
	}
	// The dispatcher returns only after the service has fully
	// stopped; ServiceMain recorded the daemon's result.
	if host.runErr != nil {
		return e.fail("serve: %v", host.runErr)
	}
	return 0
}

// callErrno extracts a Win32 error code from the trailing
// error a LazyProc.Call returns (a syscall.Errno on Windows).
func callErrno(err error) uint32 {
	if errno, ok := err.(syscall.Errno); ok {
		return uint32(errno)
	}
	return 0
}

// serviceMain is the ServiceMain callback the SCM invokes on
// its own thread once the dispatcher connects (D3).
func (h *serviceHost) serviceMain(argc uint32, argv **uint16) uintptr {
	ctx := context.Background()
	namePtr, err := syscall.UTF16PtrFromString(serviceName)
	if err != nil {
		h.runErr = fmt.Errorf("filesync: service name: %w", err)
		return 0
	}
	handle, _, e1 := procRegisterServiceCtrlHandlerExW.Call(
		uintptr(unsafe.Pointer(namePtr)), svcHandlerCallback, 0)
	if handle == 0 {
		h.runErr = fmt.Errorf("filesync: register service control handler failed (error %d)", callErrno(e1))
		return 0
	}
	h.mu.Lock()
	h.handle = handle
	h.mu.Unlock()

	if !h.setStatus(stateStartPending, 0, 1, startWaitHintMs, 0) {
		h.log.Warn(ctx, "serve: reporting START_PENDING to SCM failed")
	}
	runCtx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.cancel = cancel
	h.mu.Unlock()

	runErr := h.d.RunWithReady(runCtx, func() {
		if !h.setStatus(stateRunning, acceptStop|acceptShutdown, 0, 0, 0) {
			h.log.Warn(ctx, "serve: reporting RUNNING to SCM failed")
		}
	})
	cancel()
	h.runErr = runErr
	exitCode := uint32(0)
	if runErr != nil {
		// Startup or transport failure: report a non-zero exit
		// so the SCM/Windows logs a real failure instead of the
		// silent START_PENDING hang this job fixes.
		exitCode = 1
	}
	if !h.setStatus(stateStopped, 0, 0, 0, exitCode) {
		h.log.Warn(ctx, "serve: reporting STOPPED to SCM failed")
	}
	return 0
}

// serviceCtrlHandler receives SCM control codes for the
// service (D3): STOP/SHUTDOWN cancel the daemon context so the
// daemon takes its normal graceful shutdown path; INTERROGATE
// just re-reports the current state.
func serviceCtrlHandler(ctrl uint32, _ uint32, _ uintptr, _ uintptr) uintptr {
	h := activeServiceHost
	if h == nil {
		return 0
	}
	switch ctrl {
	case ctrlStop, ctrlShutdown:
		h.setStatus(stateStopPending, 0, 0, stopWaitHintMs, 0)
		h.mu.Lock()
		cancel := h.cancel
		h.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return 0
	case ctrlInterrogate:
		h.mu.Lock()
		state := h.state
		h.mu.Unlock()
		accepted := uint32(0)
		if state == stateRunning {
			accepted = acceptStop | acceptShutdown
		}
		h.setStatus(state, accepted, 0, 0, 0)
		return 0
	default:
		return errCallNotImplemented
	}
}

// setStatus records and reports one service state. It returns
// false when there is no status handle yet or the SCM call
// failed; the caller decides whether that is worth a warning
// (nothing better exists mid-shutdown).
func (h *serviceHost) setStatus(state, accepted, checkPoint, waitHint, exitCode uint32) bool {
	h.mu.Lock()
	h.state = state
	handle := h.handle
	h.mu.Unlock()
	if handle == 0 {
		return false
	}
	st := serviceStatus{
		serviceType:      serviceWin32OwnProcess,
		currentState:     state,
		controlsAccepted: accepted,
		win32ExitCode:    exitCode,
		checkPoint:       checkPoint,
		waitHint:         waitHint,
	}
	r1, _, _ := procSetServiceStatus.Call(handle, uintptr(unsafe.Pointer(&st)))
	return r1 != 0
}

// disableConsoleQuickEdit turns off console QuickEdit for this
// process (D5): in QuickEdit "Select" state a click into the
// window freezes the process, which hung the installer on a
// real machine. Best effort by design: every failure (no
// console, redirected stdio) is silent.
func disableConsoleQuickEdit() {
	handle, _, _ := procGetStdHandle.Call(stdInputHandle)
	if handle == 0 || handle == invalidHandleVal {
		return
	}
	var mode uint32
	r1, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode)))
	if r1 == 0 {
		return
	}
	r1, _, _ = procSetConsoleMode.Call(handle, uintptr(consoleModeWithoutQuickEdit(mode)))
	_ = r1 // best effort: a failed mode set must not block serving
}
