//go:build windows

package host

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"spawnpoint/internal/lifecycle"
)

// ServiceName is the name the service is registered under (0008-L 2.14). The
// service control manager passes it back to the dispatcher, so it has to match
// the registration exactly.
const ServiceName = "SpawnPoint"

// The Windows entry points are resolved lazily rather than pulled in from
// golang.org/x/sys. The rewrite ships as a single executable and has no
// external dependencies (0006-D 2.3); everything used here is in the standard
// syscall package.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procSetConsoleCtrlHandler         = kernel32.NewProc("SetConsoleCtrlHandler")
	procStartServiceCtrlDispatcherW   = advapi32.NewProc("StartServiceCtrlDispatcherW")
	procRegisterServiceCtrlHandlerExW = advapi32.NewProc("RegisterServiceCtrlHandlerExW")
	procSetServiceStatus              = advapi32.NewProc("SetServiceStatus")
)

// Console control events (wincon.h).
const (
	ctrlCEvent        = 0
	ctrlBreakEvent    = 1
	ctrlCloseEvent    = 2
	ctrlLogoffEvent   = 5
	ctrlShutdownEvent = 6
)

// Service control manager constants (winsvc.h).
const (
	serviceWin32OwnProcess = 0x00000010

	serviceStopped      = 1
	serviceStartPending = 2
	serviceStopPending  = 3
	serviceRunning      = 4

	serviceAcceptStop     = 0x00000001
	serviceAcceptShutdown = 0x00000004

	serviceControlStop        = 1
	serviceControlInterrogate = 4
	serviceControlShutdown    = 5

	// errorServiceSpecificError marks a stop as failed, making the service
	// control manager apply its recovery actions. Exit 2 deliberately does not
	// use it; stoppedStatus documents the conditional policy.
	errorServiceSpecificError = 1066

	// errorFailedServiceControllerConnect is what the dispatcher returns when
	// the process was not started by the service control manager. That is the
	// mode probe: this error means console mode.
	errorFailedServiceControllerConnect syscall.Errno = 1063

	// startupWaitHintMS is how long the service control manager is asked to
	// wait for startup. Applying migrations to a large database is the slow
	// part; the value is generous because overshooting it aborts the start.
	startupWaitHintMS = 30000
)

// SERVICE_STATUS.
type serviceStatus struct {
	ServiceType             uint32
	CurrentState            uint32
	ControlsAccepted        uint32
	Win32ExitCode           uint32
	ServiceSpecificExitCode uint32
	CheckPoint              uint32
	WaitHint                uint32
}

// SERVICE_TABLE_ENTRYW.
type serviceTableEntry struct {
	ServiceName *uint16
	ServiceProc uintptr
}

// The operating system calls back into this process on threads it owns and
// gives no way to pass a receiver through, so the server is held here. One
// server per process, which is what the design assumes throughout.
var (
	current Server

	statusMu        sync.Mutex
	statusHandle    uintptr
	checkPoint      uint32
	currentState    uint32
	currentAccepted uint32

	exitMu   sync.Mutex
	exitCode int
)

// Run detects the mode and drives srv, returning the mode and the process exit
// code.
//
// The detection is the attempt itself: StartServiceCtrlDispatcherW succeeds and
// blocks when the process really is a service, and fails immediately with
// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT when it is not. Asking the question
// this way needs no flag and cannot disagree with reality (0008-L 2.14).
func Run(srv Server) (Mode, int) {
	current = srv

	name, err := syscall.UTF16PtrFromString(ServiceName)
	if err != nil {
		return Console, runConsole(srv)
	}
	table := []serviceTableEntry{
		{ServiceName: name, ServiceProc: syscall.NewCallback(serviceMain)},
		{}, // The table is terminated by a zeroed entry.
	}
	r, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r != 0 {
		// The dispatcher returned, which means serviceMain has finished.
		return Service, serviceExitCode()
	}
	if errno, ok := callErr.(syscall.Errno); !ok || errno != errorFailedServiceControllerConnect {
		// Some other dispatcher failure. Running in console mode is the more
		// useful outcome than refusing to start, but say so: a service that
		// silently degraded to console mode dies with its session again.
		fmt.Fprintf(os.Stderr, "SpawnPoint: service dispatcher failed (%v); continuing in console mode\n", callErr)
	}
	return Console, runConsole(srv)
}

// serviceMain is the SERVICE_MAIN_FUNCTIONW the dispatcher calls. Its arguments
// are the service's own command line, which is ignored: environment variables
// are the only configuration source (0008-L 2.14).
func serviceMain(argc, argv uintptr) uintptr {
	name, err := syscall.UTF16PtrFromString(ServiceName)
	if err != nil {
		setServiceExitCode(lifecycle.ExitUnrecoverable)
		return 0
	}
	handle, _, _ := procRegisterServiceCtrlHandlerExW.Call(
		uintptr(unsafe.Pointer(name)),
		syscall.NewCallback(serviceControlHandler),
		0,
	)
	if handle == 0 {
		// Without a handler the service cannot be stopped cleanly, so there is
		// no point running. Nothing can be reported through the service
		// control manager either.
		setServiceExitCode(lifecycle.ExitStartFailed)
		return 0
	}

	statusMu.Lock()
	statusHandle = handle
	statusMu.Unlock()

	report(serviceStartPending, 0, startupWaitHintMS)
	code, ok := current.Startup()
	if !ok {
		setServiceExitCode(code)
		reportStopped(code)
		return 0
	}
	report(serviceRunning, serviceAcceptStop|serviceAcceptShutdown, 0)

	code = current.Serve()
	setServiceExitCode(code)
	reportStopped(code)
	return 0
}

// serviceControlHandler is the HandlerEx callback.
//
// It must return quickly — the service control manager treats a slow handler as
// a hung service — so the cleanup runs on its own goroutine while STOP_PENDING
// keeps the manager waiting. serviceMain reports STOPPED once Serve returns.
func serviceControlHandler(control, eventType, eventData, context uintptr) uintptr {
	switch uint32(control) {
	case serviceControlStop, serviceControlShutdown:
		budget := lifecycle.TotalBudget
		if uint32(control) == serviceControlShutdown {
			// A machine shutdown allows far less time than a plain stop —
			// WaitToKillServiceTimeout defaults to about five seconds — so it
			// gets the same reduced budget as closing a console window
			// (0008-L 2.4.2).
			budget = lifecycle.ConsoleCloseBudget
		}
		report(serviceStopPending, 0, uint32(budget/time.Millisecond)+2000)
		go current.Shutdown(lifecycle.ReasonServiceControl, budget)
	case serviceControlInterrogate:
		reportCurrent()
	}
	return 0 // NO_ERROR
}

// report sends a status transition. CheckPoint has to advance on every pending
// report or the service control manager decides the service has stalled.
func report(state, accepted, waitHint uint32) {
	statusMu.Lock()
	defer statusMu.Unlock()
	if state == serviceStartPending || state == serviceStopPending {
		checkPoint++
	} else {
		checkPoint = 0
	}
	currentState, currentAccepted = state, accepted
	setStatus(serviceStatus{
		ServiceType:      serviceWin32OwnProcess,
		CurrentState:     state,
		ControlsAccepted: accepted,
		CheckPoint:       checkPoint,
		WaitHint:         waitHint,
	})
}

// reportStopped is the final transition. The service control manager's recovery
// actions cannot branch on a service-specific exit code: every non-zero status
// receives the same action list. Exit 2 is therefore reported as a clean stop
// to the manager (the operations log and process exit still carry 2), while 1
// and every other abnormal code are reported as failures and trigger recovery.
func reportStopped(code int) {
	statusMu.Lock()
	defer statusMu.Unlock()
	checkPoint = 0
	currentState, currentAccepted = serviceStopped, 0
	setStatus(stoppedStatus(code))
}

func stoppedStatus(code int) serviceStatus {
	status := serviceStatus{
		ServiceType:  serviceWin32OwnProcess,
		CurrentState: serviceStopped,
	}
	if code != lifecycle.ExitNormal && code != lifecycle.ExitUnrecoverable {
		status.Win32ExitCode = errorServiceSpecificError
		status.ServiceSpecificExitCode = uint32(code)
	}
	return status
}

// reportCurrent answers an INTERROGATE with the state already reported.
func reportCurrent() {
	statusMu.Lock()
	defer statusMu.Unlock()
	setStatus(serviceStatus{
		ServiceType:      serviceWin32OwnProcess,
		CurrentState:     currentState,
		ControlsAccepted: currentAccepted,
		CheckPoint:       checkPoint,
	})
}

// setStatus performs the call. The caller holds statusMu.
func setStatus(status serviceStatus) {
	if statusHandle == 0 {
		return
	}
	procSetServiceStatus.Call(statusHandle, uintptr(unsafe.Pointer(&status)))
}

func setServiceExitCode(code int) {
	exitMu.Lock()
	defer exitMu.Unlock()
	exitCode = code
}

func serviceExitCode() int {
	exitMu.Lock()
	defer exitMu.Unlock()
	return exitCode
}

// runConsole installs the console control handler and runs the server in the
// foreground.
func runConsole(srv Server) int {
	handler := syscall.NewCallback(consoleCtrlHandler)
	// Installing the handler before startup matters: a Ctrl-C during migration
	// would otherwise be handled by the Go runtime's default, which exits
	// without running the cleanup.
	if r, _, err := procSetConsoleCtrlHandler.Call(handler, 1); r == 0 {
		fmt.Fprintf(os.Stderr,
			"SpawnPoint: cannot install the console control handler (%v); "+
				"closing the window will not be recorded\n", err)
	}

	awake := make(chan struct{})
	defer close(awake)
	go keepRuntimeAwake(awake)

	code, ok := srv.Startup()
	if !ok {
		return code
	}
	return srv.Serve()
}

// keepAliveInterval only has to keep a timer pending. The runtime's check is
// whether any timer exists at all, not when it fires, so this is deliberately
// long enough to cost nothing.
const keepAliveInterval = time.Minute

// keepRuntimeAwake stops the Go runtime from mistaking a correctly idle server
// for a deadlocked one.
//
// In console mode the only thing that will ever wake the server is the console
// control handler, which Windows invokes on a thread the Go scheduler knows
// nothing about. An idle server has every goroutine parked on a channel, so the
// runtime concludes no progress is possible and throws "all goroutines are
// asleep - deadlock!". That kills the process with exit code 2 and no
// `stopping` record — a server disappearing without a trace, which is the exact
// failure this rewrite exists to remove (0004-NR 1.4).
//
// This was measured: the skeleton exited that way the first time it was left
// idle. A pending timer is enough to tell the runtime otherwise.
//
// The service path does not need this. There the main goroutine sits inside
// StartServiceCtrlDispatcherW, and a thread in a system call counts as progress.
// Once the request front end lands (T6) its accept loop will too, but relying on
// that would make an idle server's survival depend on a component that has
// nothing to do with it.
func keepRuntimeAwake(stop <-chan struct{}) {
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

// consoleCtrlHandler is the PHANDLER_ROUTINE callback.
//
// It runs on a thread the operating system creates for it and, for the close
// and logoff events, the process is terminated the moment it returns. Blocking
// here until the cleanup is done is therefore the intended shape, not a
// mistake — it is the only window in which anything can be recorded, and it is
// about five seconds wide (0004-NR 1.5, 0008-L 2.4.3).
//
// This handler is installed after the Go runtime's own, and Windows calls
// handlers most-recently-installed first, so the runtime never sees these
// events. That is deliberate: the runtime maps close, logoff and shutdown all
// onto one signal, which would lose the distinction between console_ctrl and
// console_close and their different budgets.
func consoleCtrlHandler(ctrlType uintptr) uintptr {
	reason, budget, terminates := ConsoleEvent(uint32(ctrlType))
	if reason == "" {
		return 0 // Not ours; let the next handler decide.
	}
	code := current.Shutdown(reason, budget)
	if terminates {
		// Returning from here lets Windows kill the process with an exit code
		// that is not ours. Everything the sequence had to do is already done.
		os.Exit(code)
	}
	// Interrupt and break: Serve is already unblocking, so let the process
	// unwind through main and exit normally.
	return 1
}

// ConsoleEvent maps a console control event to the shutdown reason and budget
// fixed by 0008-L 2.4.2. terminates reports whether Windows destroys the
// process as soon as the handler returns.
//
// An unrecognised event yields an empty reason: only the values in the reason
// list of 0008-L 2.13 may ever reach the log.
func ConsoleEvent(ctrlType uint32) (reason string, budget time.Duration, terminates bool) {
	switch ctrlType {
	case ctrlCEvent, ctrlBreakEvent:
		return lifecycle.ReasonConsoleCtrl, lifecycle.TotalBudget, false
	case ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent:
		return lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget, true
	default:
		return "", 0, false
	}
}
