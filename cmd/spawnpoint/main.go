// Command spawnpoint is the SpawnPoint server.
//
// It runs as a Windows service when the service control manager starts it and
// in the foreground otherwise; the mode is detected, not selected (0008-L 2.14).
// Settings come from environment variables only — see internal/config.
//
// The startup and shutdown sequences, the operations log, the mode detection,
// the database, instance issuing, the runner and the request front end are all
// wired here.
//
// The runner is wired in full: children are started into the two job objects of
// 0008-L 2.3, the shutdown sequence tears them down in order, and the server
// job is released last so the kernel covers whatever the sequence could not.
// The exit-path measurement of 0008-L 6.4 is done against tools/exitprobe,
// which wires the same packages together and registers its children from the
// environment instead.
//
// Binding is a startup stage of its own (0008-L 2.15) and happens before
// anything is served. That ordering is what makes an occupied port a recorded
// failure with exit code 1 rather than a process that starts, prints a banner
// and never receives a request (0004-NR F3, 0008-L E-26).
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"spawnpoint/internal/config"
	"spawnpoint/internal/dialect"
	"spawnpoint/internal/host"
	"spawnpoint/internal/httpapi"
	"spawnpoint/internal/issuer"
	"spawnpoint/internal/lifecycle"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
	"spawnpoint/internal/store"
	"spawnpoint/internal/webui"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Everything before the operations log is open can only report to stderr,
	// which is why 0008-L 2.15 puts so little in front of it.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "SpawnPoint: invalid configuration: %v\n", err)
		return lifecycle.ExitUnrecoverable
	}
	log, err := opslog.Open(cfg.LogDir)
	if err != nil {
		// A server that cannot record why it died is the problem this rewrite
		// exists to remove, so it must not be left resident (0008-L E-27).
		fmt.Fprintf(os.Stderr, "SpawnPoint: %v\n", err)
		return lifecycle.ExitUnrecoverable
	}
	defer log.Close() // Idempotent; the shutdown sequence closes it at step ⑦.

	db := &database{cfg: cfg}
	// The runner is built here but has no children until something asks for
	// one, so the process-wide job object is not created either (0008-L 2.3).
	// It reads the registry through db, which is only open from the
	// OpenDatabase stage onwards — the restore stage that uses it runs after.
	procs := runner.New(cfg.LogDir, log, db, cfg.KillChildrenOnExit)
	instances := issuer.New(db, log)
	api := httpapi.New(httpapi.Options{
		Config: cfg,
		Log:    log,
		Runner: procs,
		Issuer: instances,
		Index:  webui.Index(),
	})
	front := &frontEnd{cfg: cfg, api: api}
	srv := lifecycle.New(cfg, log, lifecycle.Hooks{
		ValidateAssets:  db.validateAssets,
		OpenDatabase:    db.open,
		ApplyMigrations: db.migrate,
		RestoreEntries:  procs.Restore,
		CloseDatabase:   db.close,
		Bind:            front.bind,
		Serve:           front.serve,
		StopAccepting:   front.stopAccepting,
		WaitInflight:    front.waitInflight,
		StopChildren:    procs.StopChildren,
		StopCollectors:  procs.StopCollectors,
		CloseServerJob:  procs.CloseServerJob,
	})

	mode, code := host.Run(srv)
	if mode == host.Console {
		// Only the console has someone reading it. The service path says
		// everything it has to say in the log.
		fmt.Fprintf(os.Stderr, "SpawnPoint: stopped, exit code %d (log: %s)\n", code, log.Path())
	}
	return code
}

// database is the startup and shutdown stages that own the store.
//
// The stages are separate hooks because the sequence treats their failures
// differently, and has to. A defect in the embedded assets produces the same
// result on every attempt, so it exits unrecoverable and the service does not
// restart; a database that cannot be opened may well open on the next try — a
// file still locked by the process that just went down, for instance — so it
// exits as a start failure and is restarted (0008-L 3.2).
type database struct {
	cfg config.Config

	mu    sync.Mutex
	store *store.Store
}

// validateAssets checks everything compiled into the executable before anything
// is opened. Running it first means a build with a malformed migration script
// never touches a database at all, and a build that lost the screen stops here
// instead of serving an empty page for as long as nobody looks.
//
// Both failures are unrecoverable by the sequence's reckoning: they come from
// the build, so every restart reproduces them exactly (0008-L 3.2).
func (d *database) validateAssets() error {
	adapter, err := dialect.Select(dialect.Default)
	if err != nil {
		return err
	}
	if err := adapter.Validate(); err != nil {
		return err
	}
	if !webui.Valid() {
		return fmt.Errorf("the embedded screen is missing or malformed (%d bytes)", webui.Size())
	}
	return nil
}

func (d *database) open() error {
	s, err := store.Open(d.cfg.DBPath)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.store = s
	d.mu.Unlock()
	return nil
}

// migrate reports the counts the `migrations` record carries: how many the
// database already had, and how many this start applied (0008-L 2.9).
func (d *database) migrate() (int, int, error) {
	s, err := d.opened()
	if err != nil {
		return 0, 0, err
	}
	return s.Migrate()
}

// The registry side of the database, which is what the runner sees
// (runner.Registry). It is reached through this type rather than through the
// store directly because the store only exists between the OpenDatabase and
// CloseDatabase stages, and the runner outlives both.
//
// A call that arrives outside that window is an error rather than a silent
// no-op: the runner reports a failed save to the caller and to the operations
// log (E-21), and a save that quietly did nothing would be reported as success.

func (d *database) SaveEntry(e store.RunnerEntry) error {
	s, err := d.opened()
	if err != nil {
		return err
	}
	return s.SaveEntry(e)
}

func (d *database) DeleteEntry(id string) error {
	s, err := d.opened()
	if err != nil {
		return err
	}
	return s.DeleteEntry(id)
}

func (d *database) ListEntries() ([]store.RunnerEntry, error) {
	s, err := d.opened()
	if err != nil {
		return nil, err
	}
	return s.ListEntries()
}

// The instance registry side of the database. Like the runner methods above,
// these calls fail instead of silently doing nothing outside the open window.
func (d *database) NextSeq(datePart string) (int, error) {
	s, err := d.opened()
	if err != nil {
		return 0, err
	}
	return s.NextSeq(datePart)
}

func (d *database) Insert(inst store.Instance) error {
	s, err := d.opened()
	if err != nil {
		return err
	}
	return s.Insert(inst)
}

func (d *database) FindActiveByKey(requestKey string, now time.Time, window time.Duration) (*store.Instance, error) {
	s, err := d.opened()
	if err != nil {
		return nil, err
	}
	return s.FindActiveByKey(requestKey, now, window)
}

// opened returns the open store, or says why there is not one.
func (d *database) opened() (*store.Store, error) {
	d.mu.Lock()
	s := d.store
	d.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("database is not open")
	}
	return s, nil
}

// close is step ⑤ of the shutdown sequence, and is also called when startup
// unwinds. Both paths can reach it, so it is written to tolerate being called
// twice and to tolerate never having been opened.
func (d *database) close() {
	d.mu.Lock()
	s := d.store
	d.store = nil
	d.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// frontEnd is the socket and the HTTP server on it.
//
// The mutex is not decorative: a console control event can arrive while startup
// is still running, which puts stopAccepting and bind on different threads at
// the same moment.
type frontEnd struct {
	cfg config.Config
	api *httpapi.Server

	mu  sync.Mutex
	ln  net.Listener
	srv *http.Server
}

// readHeaderTimeout bounds how long a connection may take to send its request
// line and headers. Without it, a connection that opens and says nothing holds
// a goroutine and a file handle for as long as it likes.
const readHeaderTimeout = 15 * time.Second

// bind acquires the address and returns it for the `listening` record.
//
// No address reuse option is set. Go does set SO_REUSEADDR on Unix, but that
// never permits a second live listener on the same address; on Windows, where
// SpawnPoint is deployed, Go sets nothing, so a port already in use fails here
// exactly as 0004-NR F3 requires.
func (f *frontEnd) bind() (string, error) {
	ln, err := net.Listen("tcp", f.cfg.Address())
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.ln = ln
	f.srv = &http.Server{
		Handler:           f.api,
		ReadHeaderTimeout: readHeaderTimeout,
	}
	f.mu.Unlock()
	return "http://" + f.cfg.Address(), nil
}

// serve answers requests until the shutdown sequence closes stop.
//
// The listener runs on its own goroutine and this one waits, rather than the
// other way round, because Serve has to return on a shutdown request and
// http.Server.Serve only returns when its listener is closed — which is step ②,
// not step ①. Returning here first lets the sequence run in its documented
// order.
//
// A listener failure is not reported as an error: by the time Serve returns,
// the only thing that closed the listener is stopAccepting. Reporting it would
// turn every ordinary shutdown into the `panic` path (0008-L 2.4.2).
func (f *frontEnd) serve(stop <-chan struct{}) error {
	f.mu.Lock()
	srv, ln := f.srv, f.ln
	f.mu.Unlock()
	if srv == nil || ln == nil {
		return fmt.Errorf("the listener was never bound")
	}
	go srv.Serve(ln)
	<-stop
	return nil
}

// stopAccepting is step ② of the shutdown sequence: the socket closes, so no
// further connection is accepted, while the ones already open keep going until
// waitInflight deals with them.
func (f *frontEnd) stopAccepting() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.api.BeginShutdown()
	if f.ln != nil {
		f.ln.Close()
	}
}

// waitInflight gives requests already in progress their grace period (E-29).
//
// Shutdown waits for every connection to go idle and then closes it. When the
// budget runs out it stops waiting; the connections are closed either way, so a
// client that hangs on to one cannot hold the shutdown open past its budget.
func (f *frontEnd) waitInflight(budget time.Duration) {
	f.mu.Lock()
	srv := f.srv
	f.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		srv.Close()
	}
}
