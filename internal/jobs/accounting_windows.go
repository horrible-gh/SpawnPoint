//go:build windows

package jobs

import (
	"fmt"
	"unsafe"
)

var procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")

type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// ActiveProcesses counts the whole job, not just the original supervisor.
// Call only after assignment: a suspended process already counts as active.
func (g *Group) ActiveProcesses() (uint32, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.handle == 0 {
		return 0, fmt.Errorf("job object is closed")
	}
	var info jobBasicAccountingInformation
	r, _, err := procQueryInformationJobObject.Call(uintptr(g.handle), 1,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
	if r == 0 {
		return 0, fmt.Errorf("query job process count: %w", err)
	}
	return info.ActiveProcesses, nil
}
