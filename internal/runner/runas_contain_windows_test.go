//go:build windows

package runner

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"spawnpoint/internal/runas"
)

func suspendedIdentityProbe(t *testing.T) *syscall.ProcessInformation {
	t.Helper()
	shell, err := syscall.UTF16PtrFromString(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"))
	if err != nil {
		t.Fatal(err)
	}
	line, _ := syscall.UTF16PtrFromString("cmd.exe /d /c exit 0")
	si := syscall.StartupInfo{Cb: uint32(unsafe.Sizeof(syscall.StartupInfo{}))}
	pi := new(syscall.ProcessInformation)
	if err := syscall.CreateProcess(shell, line, nil, nil, false, 4|0x08000000, nil, nil, &si, pi); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		syscall.TerminateProcess(pi.Process, 1)
		syscall.WaitForSingleObject(pi.Process, 5000)
		syscall.CloseHandle(pi.Thread)
		syscall.CloseHandle(pi.Process)
	})
	return pi
}

func TestIdentityContainmentRejectsInvalidHandle(t *testing.T) {
	m, _ := testManager(t, nil)
	defer m.CloseServerJob()
	if job, err := m.containIdentityChild(&runas.Child{}); job != nil || err == nil {
		t.Fatalf("invalid handle was accepted: %v / %v", job, err)
	}
	if len(m.identityServerJobs) != 0 {
		t.Fatal("failed assignment retained a lifetime job")
	}
}

func TestLiveIdentityIndependentLifetimeJobs(t *testing.T) {
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("requires isolated process execution")
	}
	m, _ := testManager(t, nil)
	defer m.CloseServerJob()
	ordinary := suspendedIdentityProbe(t)
	group := m.contain(int(ordinary.ProcessId))
	if group == nil {
		t.Fatal("ordinary process containment unavailable")
	}
	defer group.Close()
	var probes []*syscall.ProcessInformation
	for i := 0; i < 2; i++ {
		pi := suspendedIdentityProbe(t)
		probes = append(probes, pi)
		child, err := m.containIdentityChild(&runas.Child{PID: int(pi.ProcessId), Handle: uintptr(pi.Process)})
		if err != nil {
			t.Fatal(err)
		}
		// Releasing the stop handle must not end a running process; the outer
		// lifetime job remains until the whole tree exits or server shutdown.
		child.Close()
		if state, _ := syscall.WaitForSingleObject(pi.Process, 0); state != syscall.WAIT_TIMEOUT {
			t.Fatal("closing stop handle terminated a live process")
		}
	}
	m.mu.Lock()
	count := len(m.identityServerJobs)
	m.mu.Unlock()
	if count != 2 {
		t.Fatalf("lifetime jobs = %d, want two independent jobs", count)
	}
	m.CloseServerJob()
	for _, pi := range append(probes, ordinary) {
		if state, err := syscall.WaitForSingleObject(pi.Process, 5000); err != nil || state != syscall.WAIT_OBJECT_0 {
			t.Fatalf("shutdown did not terminate a contained process: %v / %v", state, err)
		}
	}
}

func TestLiveIdentityEmptyLifetimeJobsAreReaped(t *testing.T) {
	if os.Getenv("SPAWNPOINT_LIVE_SPAWN") != "1" {
		t.Skip("requires isolated process execution")
	}
	m, _ := testManager(t, nil)
	defer m.CloseServerJob()
	pi := suspendedIdentityProbe(t)
	child, err := m.containIdentityChild(&runas.Child{PID: int(pi.ProcessId), Handle: uintptr(pi.Process)})
	if err != nil {
		t.Fatal(err)
	}
	child.Terminate()
	child.Close()
	syscall.WaitForSingleObject(pi.Process, 5000)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		count := len(m.identityServerJobs)
		m.mu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("empty lifetime job was not reaped")
}
