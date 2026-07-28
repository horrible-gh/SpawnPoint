// Package jobs contains children so that no path out of the server leaves one
// behind.
//
// 0008-L 2.3 asks for two layers, and they answer different questions:
//
//   - The server group is created once and holds every child. Its one property
//     is that the kernel kills everything inside it when the last handle
//     closes, which covers the paths the server never gets to run code on —
//     killed from the task manager, killed by the operating system for
//     overrunning a shutdown budget, killed by a bug. It is released last
//     (0008-L 2.4.1 step ⑧) precisely so it covers every earlier step.
//   - A child group holds one child and its descendants. It exists so that one
//     stop request tears down a whole tree at once, rather than killing a shell
//     and orphaning what the shell started. Every registered command is a shell
//     line, so the tree is never one process.
//
// A child is put into the server group first and its own group second. That is
// the reverse of the order written in 0008-L 2.3, and it is the order that
// produces the nesting the document describes: Windows makes the *second* job
// the child of the first, so the group assigned first is the outer one. Taking
// the document literally contains the first child correctly and then refuses
// every child after it, because the server group cannot be the inner job of two
// unrelated child groups at once. See the work report for this stage; the
// behaviour was measured, not inferred.
//
// Where nesting is refused the child groups are given up and the server group
// kept, since the server group is the one that cannot be reconstructed after
// the fact (0008-L 2.3 rule 3, E-19). Assigning it first is what makes that
// fallback reachable at all.
package jobs

import "errors"

// ErrNestingUnsupported is returned when a process is already in a group and
// the operating system will not add it to a second one. The caller downgrades:
// server group only, and stops go through TerminateTree.
var ErrNestingUnsupported = errors.New("nested process groups are not supported")
