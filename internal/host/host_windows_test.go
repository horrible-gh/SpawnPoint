//go:build windows

package host

import (
	"syscall"
	"testing"
	"time"
	"unsafe"

	"spawnpoint/internal/lifecycle"
)

// 0008-L 2.4.2 gives close and logoff a different budget from interrupt and
// break. The Go runtime's own handler collapses all five onto one signal, which
// is why this package installs its own — if this mapping ever flattens, that
// reason is gone.
func TestConsoleEventMapping(t *testing.T) {
	cases := []struct {
		name       string
		ctrlType   uint32
		reason     string
		budget     time.Duration
		terminates bool
	}{
		{"ctrl-c", ctrlCEvent, lifecycle.ReasonConsoleCtrl, lifecycle.TotalBudget, false},
		{"ctrl-break", ctrlBreakEvent, lifecycle.ReasonConsoleCtrl, lifecycle.TotalBudget, false},
		{"close", ctrlCloseEvent, lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget, true},
		{"logoff", ctrlLogoffEvent, lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget, true},
		{"shutdown", ctrlShutdownEvent, lifecycle.ReasonConsoleClose, lifecycle.ConsoleCloseBudget, true},
	}
	for _, c := range cases {
		reason, budget, terminates := ConsoleEvent(c.ctrlType)
		if reason != c.reason || budget != c.budget || terminates != c.terminates {
			t.Errorf("ConsoleEvent(%s) = (%q, %v, %v), want (%q, %v, %v)",
				c.name, reason, budget, terminates, c.reason, c.budget, c.terminates)
		}
	}
}

// An event outside the documented set must produce no reason at all: only the
// values listed in 0008-L 2.13 may be written to the log.
func TestUnknownConsoleEventIsNotHandled(t *testing.T) {
	for _, ctrlType := range []uint32{3, 4, 7, 99} {
		if reason, _, _ := ConsoleEvent(ctrlType); reason != "" {
			t.Errorf("ConsoleEvent(%d) = %q, want it to be left unhandled", ctrlType, reason)
		}
	}
}

// 0008-L 2.4.3 requires the close and logoff paths to be handled at all — the
// Python implementation registers only interrupt and break, so closing the
// window skips its cleanup. These three must be the ones that block.
func TestCloseAndLogoffAreHandledAndBlocking(t *testing.T) {
	for _, ctrlType := range []uint32{ctrlCloseEvent, ctrlLogoffEvent, ctrlShutdownEvent} {
		reason, budget, terminates := ConsoleEvent(ctrlType)
		if reason == "" {
			t.Fatalf("event %d is unhandled; the close path would leave no trace", ctrlType)
		}
		if !terminates {
			t.Errorf("event %d does not block the handler; Windows would kill the "+
				"process before the sequence finishes", ctrlType)
		}
		if budget > lifecycle.ConsoleCloseBudget {
			t.Errorf("event %d budget %v exceeds what the OS allows (%v)",
				ctrlType, budget, lifecycle.ConsoleCloseBudget)
		}
	}
}

// SERVICE_STATUS is seven DWORDs; the service control manager reads it by
// offset. A layout change would be silently misread.
func TestServiceStatusLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(serviceStatus{}), uintptr(28); got != want {
		t.Fatalf("sizeof(SERVICE_STATUS) = %d, want %d", got, want)
	}
	offsets := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"ServiceType", unsafe.Offsetof(serviceStatus{}.ServiceType), 0},
		{"CurrentState", unsafe.Offsetof(serviceStatus{}.CurrentState), 4},
		{"ControlsAccepted", unsafe.Offsetof(serviceStatus{}.ControlsAccepted), 8},
		{"Win32ExitCode", unsafe.Offsetof(serviceStatus{}.Win32ExitCode), 12},
		{"ServiceSpecificExitCode", unsafe.Offsetof(serviceStatus{}.ServiceSpecificExitCode), 16},
		{"CheckPoint", unsafe.Offsetof(serviceStatus{}.CheckPoint), 20},
		{"WaitHint", unsafe.Offsetof(serviceStatus{}.WaitHint), 24},
	}
	for _, o := range offsets {
		if o.got != o.want {
			t.Errorf("offsetof(%s) = %d, want %d", o.name, o.got, o.want)
		}
	}
}

// Windows recovery actions cannot select by service-specific exit code. The
// status itself must therefore mark exit 2 as a clean, non-restartable stop and
// every other abnormal exit as a failed, restartable stop (0008-L 2.14).
func TestStoppedStatusControlsRestartPolicy(t *testing.T) {
	for _, code := range []int{lifecycle.ExitNormal, lifecycle.ExitUnrecoverable} {
		status := stoppedStatus(code)
		if status.Win32ExitCode != 0 || status.ServiceSpecificExitCode != 0 {
			t.Errorf("exit %d reported as a service failure: %+v", code, status)
		}
	}
	for _, code := range []int{lifecycle.ExitStartFailed, 3, 99} {
		status := stoppedStatus(code)
		if status.Win32ExitCode != errorServiceSpecificError || status.ServiceSpecificExitCode != uint32(code) {
			t.Errorf("exit %d did not request service recovery: %+v", code, status)
		}
	}
}

// dispatcherProbed records that the one-shot measurement below has been taken.
var dispatcherProbed bool

// The mode probe of 0008-L 2.14 rests on one measurable fact: a process that is
// not a service gets ERROR_FAILED_SERVICE_CONTROLLER_CONNECT from the
// dispatcher. The test binary is such a process, so it can measure it directly.
//
// The service main in the table is never reached — if the call were to succeed
// it would block instead of returning, and the assertion below would not run.
//
// The measurement is only valid once per process. Windows remembers that a
// process has called the dispatcher and answers a second call with
// ERROR_SERVICE_ALREADY_RUNNING, which says nothing about whether this process
// is a service. Under `go test -count=2` or higher the repeats would therefore
// fail on a fact that is not the one under test, so they are skipped.
func TestDispatcherReportsConsoleModeOutsideAService(t *testing.T) {
	if dispatcherProbed {
		t.Skip("the dispatcher answers only the first call in a process; already measured")
	}
	dispatcherProbed = true

	name, err := syscall.UTF16PtrFromString(ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	table := []serviceTableEntry{
		{ServiceName: name, ServiceProc: syscall.NewCallback(func(argc, argv uintptr) uintptr {
			t.Error("service main was called in a non-service process")
			return 0
		})},
		{},
	}
	r, _, callErr := procStartServiceCtrlDispatcherW.Call(uintptr(unsafe.Pointer(&table[0])))
	if r != 0 {
		t.Fatalf("dispatcher succeeded in a test process")
	}
	errno, ok := callErr.(syscall.Errno)
	if !ok || errno != errorFailedServiceControllerConnect {
		t.Fatalf("dispatcher error = %v (%T), want ERROR_FAILED_SERVICE_CONTROLLER_CONNECT (1063)",
			callErr, callErr)
	}
}

// The Windows entry points must all resolve, or the failure only shows up on a
// production machine.
func TestWindowsEntryPointsResolve(t *testing.T) {
	procs := []*syscall.LazyProc{
		procSetConsoleCtrlHandler,
		procStartServiceCtrlDispatcherW,
		procRegisterServiceCtrlHandlerExW,
		procSetServiceStatus,
	}
	for _, p := range procs {
		if err := p.Find(); err != nil {
			t.Errorf("%s: %v", p.Name, err)
		}
	}
}

func TestModeString(t *testing.T) {
	if Console.String() != "console" || Service.String() != "service" {
		t.Fatalf("Mode strings = %q %q", Console, Service)
	}
}
