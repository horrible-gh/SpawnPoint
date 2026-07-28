// Package webui carries the process runner screen.
//
// The screen is compiled into the executable rather than read from disk beside
// it. SpawnPoint ships as a single file (0006-D 2.3), and a screen loaded from
// a path is one more thing that can be missing, stale or half-written when the
// service starts — a failure mode that shows up as a blank page long after the
// deployment that caused it.
//
// It is one file on purpose: no separate stylesheet, no script file, no font.
// Every one of those would be a second request the server has to route and a
// second thing that can be cached out of step with the first.
package webui

import (
	_ "embed"
	"strings"
)

//go:embed index.html
var index string

// Index returns the screen.
//
// A copy is returned rather than the embedded string's bytes, because the
// caller hands it to net/http, and a handler that was given the backing array
// could write through it. The screen is 35 KiB and this happens once per
// process start.
func Index() []byte { return []byte(index) }

// Size is the length of the screen in bytes, for the `Content-Length` a caller
// may want to log without holding the body.
func Size() int { return len(index) }

// Valid reports whether the embedded screen looks like the screen.
//
// It is checked at startup alongside the SQL assets (0008-L 2.15) for the same
// reason those are: a build that embedded the wrong file, or nothing, produces
// the same result on every restart, so it must stop the process rather than
// serve a blank page indefinitely.
func Valid() bool {
	if strings.HasPrefix(index, "\ufeff") {
		// A byte order mark would be served as the first characters of the
		// document and put the browser into quirks mode.
		return false
	}
	return strings.Contains(index, "<html") && strings.Contains(index, "</html>")
}
