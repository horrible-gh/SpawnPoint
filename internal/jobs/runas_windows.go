//go:build windows

package jobs

import (
	"fmt"
)

// AssignHandle accepts the creator's already-authorized process handle. It
// neither borrows ownership nor closes the handle. Call before resuming.
func (g *Group) AssignHandle(handle uintptr) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || handle == 0 {
		return fmt.Errorf("invalid process containment handle")
	}
	r, _, err := procAssignProcessToJobObj.Call(uintptr(g.handle), handle)
	if r == 0 {
		return fmt.Errorf("assign authenticated process to job: %w", err)
	}
	return nil
}
