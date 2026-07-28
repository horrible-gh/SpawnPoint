//go:build windows

package jobs

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// These tests start real processes and are opt-in, the same way the rest of the
// live checks are:
//
//	SPAWNPOINT_LIVE_SPAWN=1 go test ./internal/jobs/
//
// They exist because everything in this package is a claim about what the
// kernel does, and no amount of reading settles those. One of them — the
// assignment order — contradicted the design document, and only measurement
// showed it.

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("set SPAWNPOINT_LIVE_SPAWN=1 to run the live process group checks")
	}
}

var procGetExitCodeProcess = kernel32.NewProc("GetExitCodeProcess")

const stillActive = 259

func alive(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if r, _, _ := procGetExitCodeProcess.Call(uintptr(handle), uintptr(unsafe.Pointer(&code))); r == 0 {
		return false
	}
	return code == stillActive
}

// spawnTree starts a shell that starts a second process, so there is a
// grandchild to lose. It returns the shell.
func spawnTree(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := &exec.Cmd{
		Path: os.Getenv("ComSpec"),
		Args: []string{os.Getenv("ComSpec")},
		SysProcAttr: &syscall.SysProcAttr{
			CmdLine:       `cmd.exe /c "ping -n 60 127.0.0.1 >nul"`,
			HideWindow:    true,
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		},
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd
}

func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(pid)
}

// The measurement that corrected 0008-L 2.3.
//
// Windows makes the second job assigned the child of the first, so assigning
// the per-child group first would make the server group an inner job of that
// one child's group. The next child's group has no relationship to it, and the
// assignment is refused. Server group first, child group second, and every
// child nests correctly.
//
// This is pinned as a test because it is the kind of thing that gets "tidied"
// back into the document's order by someone reading the document.
func TestLiveAssignmentOrder(t *testing.T) {
	requireLive(t)

	t.Run("child group first fails from the second child on", func(t *testing.T) {
		server, err := NewServer()
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()

		var refused int
		for i := 0; i < 3; i++ {
			cmd := spawnTree(t)
			child, err := NewChild()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Assign(cmd.Process.Pid); err != nil {
				t.Fatalf("child %d: assigning to its own group failed: %v", i, err)
			}
			if err := server.Assign(cmd.Process.Pid); err != nil {
				if !errors.Is(err, ErrNestingUnsupported) {
					t.Fatalf("child %d: unexpected failure: %v", i, err)
				}
				refused++
			}
			child.Terminate()
			child.Close()
			cmd.Wait()
		}
		if refused == 0 {
			t.Skip("this system accepts the document's order; the swap is then only harmless")
		}
		t.Logf("the document's order was refused for %d of 3 children", refused)
	})

	t.Run("server group first works for every child", func(t *testing.T) {
		server, err := NewServer()
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()

		for i := 0; i < 3; i++ {
			cmd := spawnTree(t)
			child, err := NewChild()
			if err != nil {
				t.Fatal(err)
			}
			if err := server.Assign(cmd.Process.Pid); err != nil {
				t.Fatalf("child %d: assigning to the server group failed: %v", i, err)
			}
			if err := child.Assign(cmd.Process.Pid); err != nil {
				t.Fatalf("child %d: assigning to its own group failed: %v", i, err)
			}
			child.Terminate()
			child.Close()
			cmd.Wait()
		}
	})
}

// 0008-L 2.3: terminating a child group takes the whole subtree, not just the
// shell. Killing the shell alone would leave the process it started running
// with no way to reach it — which is the orphan this layer exists to prevent.
func TestLiveChildGroupTerminatesTheSubtree(t *testing.T) {
	requireLive(t)
	server, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	cmd := spawnTree(t)
	child, err := NewChild()
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Assign(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := child.Assign(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	grandchild := waitForGrandchild(t, cmd.Process.Pid)

	if err := child.Terminate(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	child.Close()

	if !waitGone(cmd.Process.Pid, 5*time.Second) {
		t.Errorf("the shell %d survived the group termination", cmd.Process.Pid)
	}
	if !waitGone(grandchild, 5*time.Second) {
		t.Errorf("the grandchild %d survived the group termination", grandchild)
	}
}

// 0008-L 2.3 / 2.4.1 step ⑧: closing the server group's last handle kills
// everything inside it. This is the kernel backstop, and it is what covers the
// exit paths on which the server runs no code at all.
//
// The child groups are closed while their processes are still running, which is
// what a stopped-and-restarted entry leaves behind. The backstop has to survive
// that.
func TestLiveServerGroupCloseKillsEveryone(t *testing.T) {
	requireLive(t)
	server, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}

	var shells, grandchildren []int
	for i := 0; i < 2; i++ {
		cmd := spawnTree(t)
		child, err := NewChild()
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Assign(cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		if err := child.Assign(cmd.Process.Pid); err != nil {
			t.Fatal(err)
		}
		child.Close() // released early, on purpose
		shells = append(shells, cmd.Process.Pid)
		grandchildren = append(grandchildren, waitForGrandchild(t, cmd.Process.Pid))
	}

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	for _, pid := range append(shells, grandchildren...) {
		if !waitGone(pid, 5*time.Second) {
			t.Errorf("process %d survived the server group being closed", pid)
		}
	}
}

// waitForGrandchild returns the pid of a process whose parent is pid.
func waitForGrandchild(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if child, ok := childOf(pid); ok {
			return child
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d never produced a child", pid)
	return 0
}

// PROCESSENTRY32 fields up to the parent pid; the rest is not read.
type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

// childOf finds one process whose parent is pid, by walking the process
// snapshot. tasklist would need parsing and would not report the parent at all.
func childOf(pid int) (int, bool) {
	const th32csSnapProcess = 0x00000002
	snapshot, err := syscall.CreateToolhelp32Snapshot(th32csSnapProcess, 0)
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(snapshot)

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := process32First(snapshot, &entry); err != nil {
		return 0, false
	}
	for {
		if entry.ParentProcessID == uint32(pid) {
			return int(entry.ProcessID), true
		}
		if err := process32Next(snapshot, &entry); err != nil {
			return 0, false
		}
	}
}

var (
	procProcess32FirstW = kernel32.NewProc("Process32FirstW")
	procProcess32NextW  = kernel32.NewProc("Process32NextW")
)

func process32First(snapshot syscall.Handle, entry *processEntry32) error {
	r, _, err := procProcess32FirstW.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	if r == 0 {
		return err
	}
	return nil
}

func process32Next(snapshot syscall.Handle, entry *processEntry32) error {
	r, _, err := procProcess32NextW.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	if r == 0 {
		return err
	}
	return nil
}
