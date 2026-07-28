//go:build windows

package jobs

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows entry points, resolved lazily out of the standard syscall package.
// The rewrite ships as one executable with no external dependencies
// (0006-D 2.3), and internal/host already establishes this shape.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW        = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObj   = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject      = kernel32.NewProc("TerminateJobObject")
	procIsProcessInJob          = kernel32.NewProc("IsProcessInJob")
)

const (
	// JobObjectExtendedLimitInformation.
	jobObjectExtendedLimitInformation = 9
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE: the property that makes the server
	// group a kernel backstop.
	jobObjectLimitKillOnJobClose = 0x00002000

	// Rights needed to put a process into a job and to terminate it.
	processSetQuota  = 0x0100
	processTerminate = 0x0001
	processQueryInfo = 0x0400
)

// JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// IO_COUNTERS.
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// JOBOBJECT_EXTENDED_LIMIT_INFORMATION.
type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Group is one job object.
type Group struct {
	mu     sync.Mutex
	handle syscall.Handle
	closed bool
}

// NewServer creates the process-wide group with kill-on-close set.
func NewServer() (*Group, error) {
	g, err := create()
	if err != nil {
		return nil, err
	}
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, callErr := procSetInformationJobObject.Call(
		uintptr(g.handle),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if r == 0 {
		g.Close()
		return nil, fmt.Errorf("set kill-on-close on the server job: %w", callErr)
	}
	return g, nil
}

// NewChild creates a group for one child and its descendants.
//
// Kill-on-close is deliberately not set here. A child group's handle is
// released as soon as the child is confirmed gone, and with kill-on-close that
// release would itself be a termination — of nothing, on a good day, and of a
// process that had just been restarted under the same entry on a bad one
// (0008-L 2.3 rule 2).
func NewChild() (*Group, error) { return create() }

func create() (*Group, error) {
	handle, _, callErr := procCreateJobObjectW.Call(0, 0)
	if handle == 0 {
		return nil, fmt.Errorf("create job object: %w", callErr)
	}
	return &Group{handle: syscall.Handle(handle)}, nil
}

// Assign puts the process into the group.
//
// The handle is opened from the pid rather than taken from the exec.Cmd,
// because os.Process does not expose the handle it already holds. That leaves a
// window between the process starting and this call in which a descendant it
// spawns would be created outside the group. The window is the few microseconds
// between exec.Cmd.Start returning and this line; a shell needs orders of
// magnitude longer to reach its first CreateProcess, and the current
// implementation has the same window (spawnpoint/runner.py _bind_to_job) with
// no observed escape. Contains reports whether the assignment actually took, so
// the case is detectable rather than assumed.
func (g *Group) Assign(pid int) error {
	handle, err := syscall.OpenProcess(processSetQuota|processTerminate|processQueryInfo, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(handle)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("job object is closed")
	}
	r, _, callErr := procAssignProcessToJobObj.Call(uintptr(g.handle), uintptr(handle))
	if r != 0 {
		return nil
	}
	// ERROR_ACCESS_DENIED is what a system without nested job support returns
	// for a process that is already in a job. Naming it lets the caller
	// downgrade instead of treating the child as unstartable.
	if errno, ok := callErr.(syscall.Errno); ok && errno == syscall.ERROR_ACCESS_DENIED {
		return ErrNestingUnsupported
	}
	return fmt.Errorf("assign process %d to job: %w", pid, callErr)
}

// Contains reports whether the process is in any job at all.
//
// Windows offers no way to ask "is this process in *this* job" without holding
// the job handle open on the queried side, so this is the weaker question. It
// is still worth asking: a false answer immediately after Assign means the
// process escaped, which is the failure the group exists to prevent.
func Contains(pid int) (bool, error) {
	handle, err := syscall.OpenProcess(processQueryInfo, false, uint32(pid))
	if err != nil {
		return false, err
	}
	defer syscall.CloseHandle(handle)

	var in int32
	r, _, callErr := procIsProcessInJob.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&in)))
	if r == 0 {
		return false, callErr
	}
	return in != 0, nil
}

// Terminate kills every process in the group, descendants included. Windows has
// one verb for this, so Kill is the same call.
func (g *Group) Terminate() error { return g.terminate() }

// Kill is the forced second attempt of 0008-L 2.3. On Windows it is identical
// to Terminate — TerminateJobObject is already unconditional — and exists so
// the two-stage stop reads the same on both platforms.
func (g *Group) Kill() error { return g.terminate() }

func (g *Group) terminate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.handle == 0 {
		return nil
	}
	// Exit code 1 rather than 0: these processes did not finish, and a reader
	// of the entry should not see a code that says they did.
	if r, _, callErr := procTerminateJobObject.Call(uintptr(g.handle), 1); r == 0 {
		return fmt.Errorf("terminate job object: %w", callErr)
	}
	return nil
}

// Close releases the handle. For the server group this is the moment the
// kernel backstop fires, which is why the shutdown sequence does it last.
func (g *Group) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.handle == 0 {
		return nil
	}
	g.closed = true
	err := syscall.CloseHandle(g.handle)
	g.handle = 0
	return err
}

// TerminateTree is the downgrade path for systems without nested groups
// (0008-L 2.3 rule 3). taskkill /T walks the parent-child chain, which is
// weaker than a job object — it misses a grandchild whose parent already
// exited — but it is what the current implementation uses and it is the only
// tree-wide verb available without a job.
func TerminateTree(pid int) error {
	cmd := exec.Command("taskkill", "/PID", fmt.Sprint(pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("taskkill %d: %w: %s", pid, err, out)
	}
	return nil
}
