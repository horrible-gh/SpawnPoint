// Command exitprobe is the measurement harness for the exit-path test of
// 0008-L 6.4.
//
// That test asks for a rewrite server started in console mode with two children
// registered and running, then killed five different ways. Registering a
// command is an HTTP request and the request front end is the next work item
// but one (0009-CH T6), so today there is no way to get a child into the real
// executable. This program closes that gap: it wires config, opslog, lifecycle,
// host and runner together exactly as cmd/spawnpoint does and takes its
// commands from SPAWNPOINT_PROBE_COMMANDS instead of from a request.
//
// Everything the measurement is about is therefore the shipped code — the same
// startup order, the same console control handler, the same job objects, the
// same shutdown sequence. What differs is the one component that does nothing
// yet. When T6 lands, 6.4 should be re-run against cmd/spawnpoint itself; until
// then this is the closest a measurement can get, and it is closer than not
// measuring.
//
// It is not shipped. It lives under tools/ alongside the reference dumpers for
// the same reason: it exists to produce evidence, not to be deployed.
package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"spawnpoint/internal/config"
	"spawnpoint/internal/host"
	"spawnpoint/internal/lifecycle"
	"spawnpoint/internal/opslog"
	"spawnpoint/internal/runner"
)

// EnvCommands holds the shell lines to register, one per line. Blank lines are
// skipped so the value can be written as a here-document.
const EnvCommands = "SPAWNPOINT_PROBE_COMMANDS"

// ReadyFile is where the probe reports what it started: one `<id> <status>
// <pid>` line per command, written once every command has been registered.
func ReadyFile(logDir string) string { return filepath.Join(logDir, "probe-ready.txt") }

func main() { os.Exit(run()) }

func run() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "exitprobe: invalid configuration: %v\n", err)
		return lifecycle.ExitUnrecoverable
	}
	log, err := opslog.Open(cfg.LogDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "exitprobe: %v\n", err)
		return lifecycle.ExitUnrecoverable
	}
	defer log.Close()

	// No registry: the probe measures exit paths, and persistence has its own
	// tests. Leaving the database out also keeps the measurement from being
	// confused by a slow migration.
	procs := runner.New(cfg.LogDir, log, nil, cfg.KillChildrenOnExit)
	front := &frontEnd{cfg: cfg, procs: procs}
	srv := lifecycle.New(cfg, log, lifecycle.Hooks{
		RestoreEntries: procs.Restore,
		Bind:           front.bind,
		Serve:          front.serve,
		StopAccepting:  front.stopAccepting,
		StopChildren:   procs.StopChildren,
		StopCollectors: procs.StopCollectors,
		CloseServerJob: procs.CloseServerJob,
	})

	_, code := host.Run(srv)
	return code
}

// frontEnd stands where the request front end will.
//
// It binds a real listener, so the startup order is the one 0008-L 2.15 fixes
// and the `listening` record is where it belongs, and it registers its commands
// from the serve stage — which is where a registration request would arrive,
// once the server is up. Registering from a goroutine alongside startup instead
// would interleave the records and measure an ordering that will never happen.
type frontEnd struct {
	cfg   config.Config
	procs *runner.Manager

	mu sync.Mutex
	ln net.Listener
}

func (f *frontEnd) bind() (string, error) {
	ln, err := net.Listen("tcp", f.cfg.Address())
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.ln = ln
	f.mu.Unlock()
	return "http://" + f.cfg.Address(), nil
}

func (f *frontEnd) serve(stop <-chan struct{}) error {
	var report strings.Builder
	for _, line := range strings.Split(os.Getenv(EnvCommands), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		info, _ := f.procs.Register(line, line, nil, nil)
		pid := 0
		if info.PID != nil {
			pid = *info.PID
		}
		fmt.Fprintf(&report, "%s %s %d\n", info.ID, info.Status, pid)
	}
	fmt.Print(report.String())
	// The same report goes to a file because the console-close measurement has
	// to give the probe a console of its own, and a process with its own
	// console has no pipe back to the test. The file is the only channel that
	// works on all five exit paths.
	os.WriteFile(ReadyFile(f.cfg.LogDir), []byte(report.String()), 0o644)

	<-stop
	return nil
}

func (f *frontEnd) stopAccepting() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		f.ln.Close()
	}
}
