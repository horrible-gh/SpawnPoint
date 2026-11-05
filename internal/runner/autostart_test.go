package runner

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnpoint/internal/store"
)

func TestAutoStartSettingsPersistWithoutStarting(t *testing.T) {
	registry := &fakeRegistry{}
	m, _ := testManager(t, registry)
	info, saved := m.RegisterOnly("automatic", "echo hello", nil, nil, true)
	if !saved || !info.AutoStart || info.Status != StatusStopped || info.PID != nil {
		t.Fatalf("save-only registration = %+v, saved=%v", info, saved)
	}
	if len(registry.saved) != 1 || !registry.saved[0].AutoStart {
		t.Fatal("automatic start preference was not persisted with registration")
	}
	info, saved, found := m.Update(info.ID, nil, nil, nil, nil)
	if !found || !saved || !info.AutoStart || info.Status != StatusStopped {
		t.Fatal("a legacy update changed auto-start or live state")
	}
	info, saved, found = m.Update(info.ID, nil, nil, nil, nil, false)
	if !found || !saved || info.AutoStart || info.Status != StatusStopped {
		t.Fatal("explicitly disabling auto-start failed")
	}
	if registry.saved[len(registry.saved)-1].AutoStart {
		t.Fatal("disabled preference was not persisted")
	}
	manual, _ := m.RegisterOnly("manual", "echo manual", nil, nil)
	if manual.AutoStart {
		t.Fatal("new registrations must default to manual start")
	}
}

func TestAutoStartRestoresFlagsAndAttemptsOnlyEnabledEntriesOnce(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	registry := &fakeRegistry{rows: []store.RunnerEntry{
		{ID: "proc_00000001", Label: "off", Cmd: "echo off", Cwd: &missing},
		{ID: "proc_00000002", Label: "on", Cmd: "echo on", Cwd: &missing, AutoStart: true},
		{ID: "proc_00000003", Label: "also on", Cmd: "echo on", Cwd: &missing, AutoStart: true},
	}}
	m, dir := testManager(t, registry)
	if n, err := m.Restore(); err != nil || n != 3 {
		t.Fatalf("Restore = %d, %v", n, err)
	}
	for _, info := range m.List() {
		if info.Status != StatusStopped || info.PID != nil {
			t.Fatal("Restore must not start a process before the listener is bound")
		}
	}
	m.AutoStart(nil)
	for _, info := range m.List() {
		want := StatusStopped
		if info.AutoStart {
			want = StatusFailed // Missing cwd: no real process is launched.
		}
		if info.Status != want {
			t.Errorf("%s status=%s, want %s", info.ID, info.Status, want)
		}
	}
	before := opsLog(t, dir)
	if strings.Count(before, "INFO automatic start") != 2 {
		t.Fatalf("expected both enabled attempts despite the first failing:\n%s", before)
	}
	m.AutoStart(nil)
	if after := opsLog(t, dir); after != before {
		t.Fatal("a second AutoStart call retried entries")
	}
}

func TestAutoStartDoesNotLaunchAfterCancellation(t *testing.T) {
	m, _ := testManager(t, nil)
	info, _ := m.RegisterOnly("cancelled", "echo should-not-run", nil, nil, true)
	stop := make(chan struct{})
	close(stop)
	m.AutoStart(stop)
	got, _ := m.Get(info.ID)
	if got.Status != StatusStopped || got.PID != nil {
		t.Fatalf("closed stop channel still launched %+v", got)
	}
}

func TestLiveAutoStartSelectionFailureAndNoRestart(t *testing.T) {
	requireLiveSpawn(t)
	missing := filepath.Join(t.TempDir(), "missing")
	registry := &fakeRegistry{rows: []store.RunnerEntry{
		{ID: "proc_00000001", Label: "fail first", Cmd: "echo bad", Cwd: &missing, AutoStart: true},
		{ID: "proc_00000002", Label: "automatic", Cmd: sleepFor(30 * time.Second), AutoStart: true},
		{ID: "proc_00000003", Label: "manual", Cmd: sleepFor(30 * time.Second)},
	}}
	m, _ := testManager(t, registry)
	defer func() {
		m.StopChildren("test cleanup", time.Now().Add(10*time.Second))
		m.StopCollectors()
		m.CloseServerJob()
	}()
	if _, err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.AutoStart(nil)
	active, _ := m.Get("proc_00000002")
	if active.Status != StatusRunning || active.PID == nil || !alive(*active.PID) {
		t.Fatalf("enabled process did not start after an earlier failure: %+v", active)
	}
	manual, _ := m.Get("proc_00000003")
	if manual.Status != StatusStopped || manual.PID != nil {
		t.Fatal("manual entry was started")
	}
	m.AutoStart(nil)
	again, _ := m.Get(active.ID)
	if again.PID == nil || *again.PID != *active.PID {
		t.Fatal("automatic start launched a duplicate")
	}
	m.Stop(active.ID)
	m.AutoStart(nil)
	again, _ = m.Get(active.ID)
	if again.Status != StatusStopped {
		t.Fatal("automatic start revived a manually stopped entry")
	}
}

func TestLiveAutoStartLeavesAlreadyRunningEntryAlone(t *testing.T) {
	requireLiveSpawn(t)
	m, _ := testManager(t, nil)
	defer func() {
		m.StopChildren("test cleanup", time.Now().Add(10*time.Second))
		m.StopCollectors()
		m.CloseServerJob()
	}()
	before, _ := m.Register("already running", sleepFor(30*time.Second), nil, nil, true)
	if before.Status != StatusRunning || before.PID == nil {
		t.Fatalf("start failed: %+v", before)
	}
	m.AutoStart(nil)
	after, _ := m.Get(before.ID)
	if after.PID == nil || *after.PID != *before.PID {
		t.Fatal("startup replaced an already running process")
	}
}
