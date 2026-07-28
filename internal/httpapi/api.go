// Package httpapi is SpawnPoint's request front end.
//
// It owns everything between the socket and the runner: the route table, the
// order in which a request is judged, the authentication check, the request
// body limit, and the exact shape of every response. 0007-P fixed all of that
// as an external contract, so this package is written to be read against that
// document rather than against the Python implementation it replaces — where
// the two disagree, the places are marked.
//
// The order of the checks is itself the contract (0008-L 4.1). Body length is
// judged before the path, the path before authentication, and authentication
// before the fields, so that a large body is never read, an unknown path never
// reveals whether a token would have worked, and a bad token never produces a
// field-level complaint. serve() below is that list, in that order, and nothing
// else decides it.
package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
)

// Version is the `version` field of /healthz. It is the service's version, not
// the API's: the API is versionless by contract — 0007-P has no /api/v1 and
// adding one would break every existing caller.
const Version = "1.0.0"

// RequestBodyMaxBytes is `request_body_max_bytes` (0008-L 1.2). One limit for
// every route; /spawn and /processes do not differ (0007-P [본문 초과]).
const RequestBodyMaxBytes = 65536

// Runner is the part of the process runner this package uses. It is an
// interface so the contract tests can drive every response shape — including
// the ones that need a persistence failure or a start failure — without
// starting a process or opening a database.
type Runner interface {
	List() []runner.Info
	Register(label, cmd string, cwd *string, env map[string]string) (runner.Info, bool)
	Update(id string, label, cmd *string, cwd **string, env map[string]string) (info runner.Info, persisted, found bool)
	Run(id string) (runner.Info, bool)
	Stop(id string) (runner.Info, bool)
	Restart(id string) (runner.Info, bool)
	Delete(id string) bool
	ReadLog(id, offsetParam string) (runner.LogView, bool)
}

// Server answers requests. It is safe for concurrent use and holds no state
// that a request can change.
type Server struct {
	log    *opslog.Logger
	runner Runner
	// issuer creates instances for POST /spawn after this package has completed
	// authentication, JSON decoding and field validation.
	issuer Issuer
	index  []byte
	// tokens is the allowed Bearer list as a set. Empty means authentication is
	// off, which is the local default and must stay that way (0007-P 0.1).
	tokens map[string]struct{}

	startedAt time.Time

	// stopping is set when the shutdown sequence has begun.
	mu       sync.RWMutex
	stopping bool
}

// Options are the wiring. A nil Runner or Issuer remains useful in focused
// contract tests; production supplies both.
type Options struct {
	Config config.Config
	Log    *opslog.Logger
	Runner Runner
	Issuer Issuer
	// Index is the screen, served from memory on / and /index.html. A nil Index
	// makes those two routes 404 — they are then not defined, so the reply is
	// `Unknown endpoint.` and not an empty page.
	Index []byte
	// StartedAt is what /healthz reports. Zero means "now".
	StartedAt time.Time
}

// New builds a server.
func New(opts Options) *Server {
	tokens := make(map[string]struct{}, len(opts.Config.APITokens))
	for _, t := range opts.Config.APITokens {
		tokens[t] = struct{}{}
	}
	started := opts.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	return &Server{
		log:       opts.Log,
		runner:    opts.Runner,
		issuer:    opts.Issuer,
		index:     opts.Index,
		tokens:    tokens,
		startedAt: started,
	}
}

// BeginShutdown marks the server as winding down.
//
// From here on every answer carries `Connection: close`. Closing the listener
// stops new connections, but a caller that already has one — the screen, which
// polls every second on a kept-alive connection — would otherwise keep sending
// requests into a server that is trying to leave, and each one resets the
// connection's idle timer that the inflight wait is watching for (E-29).
func (s *Server) BeginShutdown() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
}

func (s *Server) shuttingDown() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stopping
}

// --- Routing ----------------------------------------------------------------

// route is one entry of 0007-P 0.1. methods is the complete allowed set; a
// request that matches the path but not the method is 405, which is the only
// way 405 is ever produced.
type route struct {
	kind    routeKind
	methods []string
	// id is the process identifier for the routes that carry one.
	id string
	// action is run / stop / restart.
	action string
}

type routeKind int

const (
	routeNone routeKind = iota
	routeIndex
	routeHealth
	routeSpawn
	routeProcesses
	routeProcessItem
	routeProcessAction
	routeProcessLogs
)

// authFree reports whether the route is served without a token. Three routes
// are: the screen, its alias and the liveness check (0007-P 0.1).
func (k routeKind) authFree() bool {
	return k == routeIndex || k == routeHealth
}

// resolve maps a path to a route.
//
// The screen, its alias, the liveness check and /spawn match exactly; the
// runner routes are matched on segments. That split is deliberate rather than
// tidy: matching /spawn on segments would make `/spawn/` and `/spawn/anything`
// the same endpoint, and matching the runner on strings would need one case per
// identifier.
//
// An operation name that is not one of the three defined ones is not a bad
// method on a known path — it is a path that does not exist, so it is 404 with
// `Unknown endpoint.` (E-15). `Process not found.` is reserved for a real
// operation on an identifier that is not registered, and the two are told apart
// by that message alone.
func (s *Server) resolve(path string) route {
	switch path {
	case "/", "/index.html":
		if s.index == nil {
			return route{}
		}
		return route{kind: routeIndex, methods: []string{http.MethodGet, http.MethodHead}}
	case "/healthz":
		return route{kind: routeHealth, methods: []string{http.MethodGet, http.MethodHead}}
	case "/spawn":
		return route{kind: routeSpawn, methods: []string{http.MethodPost}}
	}

	parts := segments(path)
	if len(parts) == 0 || parts[0] != "processes" {
		return route{}
	}
	switch len(parts) {
	case 1:
		return route{kind: routeProcesses, methods: []string{http.MethodGet, http.MethodHead, http.MethodPost}}
	case 2:
		return route{
			kind:    routeProcessItem,
			methods: []string{http.MethodPut, http.MethodDelete},
			id:      parts[1],
		}
	case 3:
		switch parts[2] {
		case "run", "stop", "restart":
			return route{
				kind:    routeProcessAction,
				methods: []string{http.MethodPost},
				id:      parts[1],
				action:  parts[2],
			}
		case "logs":
			return route{
				kind:    routeProcessLogs,
				methods: []string{http.MethodGet, http.MethodHead},
				id:      parts[1],
			}
		}
	}
	return route{}
}

func segments(path string) []string {
	out := make([]string, 0, 4)
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (r route) allows(method string) bool {
	for _, m := range r.methods {
		if m == method {
			return true
		}
	}
	return false
}

// --- Dispatch ---------------------------------------------------------------

// ServeHTTP runs 0008-L 4.1 in its numbered order.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown() {
		w.Header().Set("Connection", "close")
	}
	// A handler that panics is recorded and then allowed to carry on panicking.
	//
	// Recording it is the whole point of this rewrite — 0004-NR 1.4 found that
	// the current server leaves nothing behind when it fails. Not converting it
	// into a response is deliberate: 0007-P 0.2 lists six error codes and a
	// seventh invented here would be a contract this server has to keep
	// forever. net/http closes the connection instead, which is what a caller
	// already handles as "the server went away".
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Log(opslog.Error, "request failed",
				opslog.F("method", r.Method),
				opslog.F("path", r.URL.Path),
				opslog.F("detail", fmt.Sprint(rec)))
			panic(rec)
		}
	}()

	// ① Length before anything else. A body over the limit is refused without
	// being read, so an oversized request costs the memory of no part of it —
	// which is the reason the check is first rather than next to the parse.
	if r.ContentLength > RequestBodyMaxBytes {
		s.fail(w, r, tooLargeError())
		return
	}

	rt := s.resolve(r.URL.Path)

	// ② An undefined path is 404 before authentication is even looked at. A
	// server that answered 401 here would be telling an unauthenticated caller
	// which paths exist (0007-P [알 수 없는 경로]).
	if rt.kind == routeNone {
		s.fail(w, r, notFoundError(msgUnknownEndpoint))
		return
	}

	// ③ A defined path with the wrong method. No Allow header: the current
	// server does not send one and nothing reads it (0007-P [알 수 없는 경로]).
	if !rt.allows(r.Method) {
		s.fail(w, r, methodNotAllowedError())
		return
	}

	// ④ ⑤ The three unauthenticated routes, then the token check for the rest.
	if !rt.kind.authFree() && !s.authorised(r) {
		s.fail(w, r, unauthorisedError())
		return
	}

	switch rt.kind {
	case routeIndex:
		s.serveIndex(w, r)
	case routeHealth:
		s.serveHealth(w, r)
	case routeSpawn:
		s.serveSpawn(w, r)
	case routeProcesses:
		if r.Method == http.MethodPost {
			s.serveRegister(w, r)
		} else {
			s.serveList(w, r)
		}
	case routeProcessItem:
		if r.Method == http.MethodPut {
			s.serveUpdate(w, r, rt.id)
		} else {
			s.serveDelete(w, r, rt.id)
		}
	case routeProcessAction:
		s.serveAction(w, r, rt.id, rt.action)
	case routeProcessLogs:
		s.serveLogs(w, r, rt.id)
	}
}

// serveIndex sends the embedded screen.
//
// no-store rather than a validator: the screen is compiled into the binary, so
// a cached copy outlives an upgrade and talks to a server it no longer matches
// (0007-P [화면 최초 진입]).
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	writeBody(w, r, http.StatusOK, s.index)
}

// healthResponse is 0007-P [생존 확인].
type healthResponse struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	Version   string `json:"version"`
	StartedAt string `json:"started_at"`
}

func (s *Server) serveHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, healthResponse{
		OK:        true,
		Status:    "healthy",
		Version:   Version,
		StartedAt: responseTime(s.startedAt),
	})
}
