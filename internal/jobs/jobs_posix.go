//go:build !windows

package jobs

import (
	"fmt"
	"sync"
	"syscall"
)

// On POSIX the containment primitive is the process group, not a job object
// (0008-L 2.3 rule 4). Children are started with their own group id, so a child
// group is that one group id and the server group is the list of all of them.
//
// One property does not carry over: nothing in POSIX kills a process group
// because a descriptor closed. Close therefore terminates the members itself,
// which covers the ordinary exits but not the server being killed outright —
// there the children survive. SpawnPoint is deployed as a Windows service
// (0006-D 2.3); this path exists so the sequences can be developed and tested
// elsewhere, and the gap is recorded rather than papered over.
type Group struct {
	mu          sync.Mutex
	pgids       []int
	killOnClose bool
	closed      bool
}

// NewServer creates the process-wide group.
func NewServer() (*Group, error) { return &Group{killOnClose: true}, nil }

// NewChild creates a group for one child and its descendants.
func NewChild() (*Group, error) { return &Group{}, nil }

// Assign records the child's process group. The child is placed in a group of
// its own by Setpgid at spawn time, so the group id is the child's pid and
// there is nothing to move here.
func (g *Group) Assign(pid int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("process group set is closed")
	}
	g.pgids = append(g.pgids, pid)
	return nil
}

// Contains reports whether the process is contained. Every child is, by
// construction, so this is the constant the Windows check cannot be.
func Contains(int) (bool, error) { return true, nil }

// Terminate asks the members to stop.
func (g *Group) Terminate() error { return g.signal(syscall.SIGTERM) }

// Kill is the forced second attempt of 0008-L 2.3.
func (g *Group) Kill() error { return g.signal(syscall.SIGKILL) }

func (g *Group) signal(sig syscall.Signal) error {
	g.mu.Lock()
	pgids := append([]int(nil), g.pgids...)
	g.mu.Unlock()

	var firstErr error
	for _, pgid := range pgids {
		// ESRCH means the group is already gone, which is the outcome asked
		// for; anything else is worth reporting.
		if err := syscall.Kill(-pgid, sig); err != nil && err != syscall.ESRCH && firstErr == nil {
			firstErr = fmt.Errorf("signal process group %d: %w", pgid, err)
		}
	}
	return firstErr
}

// Close releases the group. For the server group it also kills the members,
// standing in for the kill-on-close property Windows provides in the kernel.
func (g *Group) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	kill := g.killOnClose
	g.mu.Unlock()
	if kill {
		return g.signal(syscall.SIGKILL)
	}
	return nil
}

// TerminateTree is the downgrade path. POSIX always has process groups, so it
// is only reachable when a child was started without one.
func TerminateTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill process group %d: %w", pid, err)
	}
	return nil
}
