//go:build windows

package runner

import (
	"os"
	"syscall"
	"unsafe"
)

// stillActive is STILL_ACTIVE. A handle can be opened on a process that has
// already exited, so the exit code is what actually answers the question.
const stillActive = 259

// PROCESS_QUERY_INFORMATION, the only right this needs.
const queryProcess = 0x0400

var procGetExitCodeProcess = syscall.NewLazyDLL("kernel32.dll").NewProc("GetExitCodeProcess")

// processAlive reports whether the process is still running.
//
// os.FindProcess never fails on Windows and os.Process.Signal cannot ask this,
// so the question goes to the kernel directly. A pid that cannot be opened has
// gone and been reaped, which is the answer wanted here.
func processAlive(p *os.Process) bool {
	handle, err := syscall.OpenProcess(queryProcess, false, uint32(p.Pid))
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
