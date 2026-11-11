// Package logretention is the shared retention policy for child and server logs.
// Both use a rolling size/day boundary with ten archived generations and a
// thirty-day age ceiling. The active log is never removed while it is open.
package logretention

import (
	"os"
	"strconv"
	"time"
)

const (
	ArchiveKeep = 10
	KeepDays    = 30
	KeepFor     = KeepDays * 24 * time.Hour
)

// DifferentDay compares local calendar dates, not elapsed 24-hour intervals.
func DifferentDay(previous, now time.Time) bool {
	if previous.IsZero() {
		return false
	}
	a, b := previous.In(time.Local), now.In(time.Local)
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 != y2 || m1 != m2 || d1 != d2
}

func Expired(modified, now time.Time) bool {
	return !modified.After(now.Add(-KeepFor))
}

// Prune removes only numbered archive files, never the active log. Unknown
// filenames and non-regular files are deliberately ignored.
func Prune(base string, keep int, now time.Time) {
	for i := 1; i <= keep; i++ {
		path := base + "." + strconv.Itoa(i)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if Expired(info.ModTime(), now) {
			_ = os.Remove(path)
		}
	}
	// A process killed mid-rename may leave its temporary archive behind.
	pending := base + ".rotating"
	if info, err := os.Lstat(pending); err == nil && info.Mode().IsRegular() && Expired(info.ModTime(), now) {
		_ = os.Remove(pending)
	}
}

// ShiftCurrent archives a closed file without creating an empty active file.
// Rename the current file first: a Windows reader may prevent rename, in which
// case no older archive has been deleted merely because a rotation was tried.
func ShiftCurrent(base string, keep int) error {
	pending := base + ".rotating"
	// Recover any archive whose rename was interrupted by a service crash.
	// If a new current file appeared meanwhile, archive the older pending file
	// first. Never overwrite the pending bytes or leave rotation disabled forever.
	if pendingInfo, err := os.Lstat(pending); err == nil {
		if !pendingInfo.Mode().IsRegular() {
			return os.ErrInvalid
		}
		if _, curErr := os.Lstat(base); os.IsNotExist(curErr) {
			if err := os.Rename(pending, base); err != nil {
				return err
			}
		} else if curErr == nil {
			if _, occupied := os.Stat(base + ".1"); occupied == nil {
				shiftArchives(base, keep)
			}
			if err := os.Rename(pending, base+".1"); err != nil {
				return err
			}
		} else {
			return curErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(base, pending); err != nil {
		return err
	}
	shiftArchives(base, keep)
	if err := os.Rename(pending, base+".1"); err != nil {
		_ = os.Rename(pending, base) // Preserve bytes if an archive rename fails.
		return err
	}
	return nil
}

func shiftArchives(base string, keep int) {
	_ = os.Remove(base + "." + strconv.Itoa(keep))
	for n := keep - 1; n >= 1; n-- {
		from := base + "." + strconv.Itoa(n)
		to := base + "." + strconv.Itoa(n+1)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
}
