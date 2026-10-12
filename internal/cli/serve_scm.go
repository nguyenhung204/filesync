package cli

// Pure, platform-neutral pieces of the Windows service (SCM)
// integration: the mode decision out of StartServiceCtrlDispatcher
// and the console-mode bit math for disabling QuickEdit. The
// syscall side lives in serve_windows.go; keeping the decisions
// here makes them unit-testable on Linux, where no SCM exists.
// The setup binary carries its own small copy of the console-mode
// helper next to its own syscalls (deliberate duplication, the
// psQuote precedent from job 007: package main cannot be imported).

// errFailedServiceControllerConnect is the error
// StartServiceCtrlDispatcherW reports when the process was NOT
// started by the Service Control Manager
// (ERROR_FAILED_SERVICE_CONTROLLER_CONNECT).
const errFailedServiceControllerConnect = 1063

// serveMode is the outcome of attempting to connect to the SCM.
type serveMode int

const (
	serveModeService serveMode = iota // dispatcher connected: real service mode
	serveModeConsole                  // error 1063: run in the foreground as before
	serveModeFail                     // any other dispatcher error: give up loudly
)

// serveModeForDispatch maps the StartServiceCtrlDispatcherW
// outcome to the mode decision (job 008 spec D2): 0 means the
// call succeeded (service mode), 1063 means the SCM did not
// start this process (console mode), anything else is fatal.
func serveModeForDispatch(errCode uint32) serveMode {
	switch errCode {
	case 0:
		return serveModeService
	case errFailedServiceControllerConnect:
		return serveModeConsole
	default:
		return serveModeFail
	}
}

// Console mode bits (wincon.h). ENABLE_EXTENDED_FLAGS must be
// set for the QuickEdit bit change to take effect.
const (
	enableQuickEditMode = 0x0040
	enableExtendedFlags = 0x0080
)

// consoleModeWithoutQuickEdit returns the console input mode
// with QuickEdit disabled. A console in QuickEdit "Select"
// state freezes the owning process when the user clicks into
// the window — the exact hang the installer hit on a real
// machine (job 008 spec D5). Every other bit is preserved.
func consoleModeWithoutQuickEdit(mode uint32) uint32 {
	return (mode | enableExtendedFlags) &^ enableQuickEditMode
}
