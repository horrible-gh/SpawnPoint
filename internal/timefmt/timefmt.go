// Package timefmt renders the two timestamp shapes SpawnPoint uses.
//
// 0007-P 0.3 fixed the shapes as an external contract and 0008-L 1.6 fixed how
// the digits are produced. Both are truncating, never rounding:
//
//	response  2026-07-28T06:50:52.117482+09:00   local offset, 6 fraction digits
//	storage   2026-07-28T05:32:10.482Z           UTC, 3 fraction digits
//
// The storage form must stay fixed-width and truncated because the dedup window
// compares it as a string (`created_at > ?`, 0008-L 2.12). Rounding would flip
// the comparison against rows the Python implementation wrote.
package timefmt

import "time"

// responseLayout keeps the fraction digits even when they are zero — `.000000`
// rather than Go's (and Python isoformat's) omission. 0007-P 0.3 chose the
// fixed-width side of that difference explicitly.
const responseLayout = "2006-01-02T15:04:05.000000-07:00"

// storageLayout omits the zone on purpose: Go reads a bare `Z` in a layout as
// part of the `Z07:00` token, so the suffix is appended as a literal instead.
const storageLayout = "2006-01-02T15:04:05.000"

// Response renders t for API responses and the operations log, in local time.
func Response(t time.Time) string {
	return t.Local().Format(responseLayout)
}

// Storage renders t for the database columns, in UTC.
func Storage(t time.Time) string {
	return t.UTC().Format(storageLayout) + "Z"
}
