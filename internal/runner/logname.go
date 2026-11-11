package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"spawnpoint/internal/opslog"
	"spawnpoint/internal/store"
)

// logStem converts a display label to a portable filename. The internal process
// ID stays in the DB/API but is not needed in the user's log folder.
// Windows-invalid characters, device names, separators and trailing dots/spaces
// are filtered even on Linux, because a deployment may later be moved.
func logStem(label string) string {
	label = strings.TrimSpace(label)
	var b strings.Builder
	for _, r := range label {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			r = '_'
		}
		if b.Len()+utf8.RuneLen(r) > 88 {
			break
		}
		b.WriteRune(r)
	}
	value := strings.Trim(b.String(), ". ")
	if value == "" {
		value = "Process"
	}
	stem := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
		(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
		value = "Process-" + value
	}
	if strings.EqualFold(value, "spawnpoint") {
		value = "SpawnPoint (process)"
	}
	return value
}

func restoredLogName(stored, id string) string {
	if stored != "" && logStem(stored) == stored {
		return stored
	}
	// Pre-007 registrations have an ID-based on-disk name.
	return id
}

// logPathLocked requires e.mu. Public LogPath looks up and locks the entry.
func (m *Manager) logPathLocked(e *entry) string {
	name := e.logName
	if name == "" {
		name = e.id
	}
	return filepath.Join(m.logDir, name+".log")
}

func (m *Manager) rowLocked(e *entry) store.RunnerEntry {
	return store.RunnerEntry{
		ID: e.id, Label: e.label, LogName: e.logName, Cmd: e.cmd,
		Cwd: e.cwd, Env: copyEnv(e.env), AutoStart: e.autoStart,
		RunAsJSON: e.runAs.JSON(), CreatedAt: e.createdAt, UpdatedAt: e.updatedAt,
	}
}

// availableLogNameLocked requires m.mu. It reserves names case-insensitively,
// also avoiding unowned files and numbered archives left by old installations.
func (m *Manager) availableLogNameLocked(label, id, own string) string {
	base := logStem(label)
	for n := 1; n < 10000; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)", base, n)
		}
		if strings.EqualFold(candidate, own) {
			return own
		}
		if strings.EqualFold(candidate, "spawnpoint") {
			continue
		}
		busy := false
		for eid, other := range m.entries {
			if eid != id && strings.EqualFold(other.logName, candidate) {
				busy = true
				break
			}
		}
		if busy || m.hasLogFiles(candidate) {
			continue
		}
		return candidate
	}
	// Extremely unlikely: keep a valid, safe legacy path rather than overwriting
	// a different process's log file when thousands of labels collide.
	if own != "" {
		return own
	}
	return id
}

func (m *Manager) hasLogFiles(stem string) bool {
	files, err := os.ReadDir(m.logDir)
	if err != nil {
		return false
	}
	base := strings.ToLower(stem + ".log")
	for _, file := range files {
		name := strings.ToLower(file.Name())
		if name == base || strings.HasPrefix(name, base+".") {
			return true
		}
	}
	return false
}

// renameLogLocked requires e.mu and the caller's e.opMu (except at startup).
// An active Windows log handle cannot be renamed safely. Defer until the next
// stop, restart, startup, or maintenance pass instead of interrupting output.
func (m *Manager) renameLogLocked(e *entry) {
	if e.status == StatusRunning {
		return
	}
	if e.coll != nil {
		select {
		case <-e.coll.done:
		default:
			return
		}
	}
	old := e.logName
	if old == "" {
		old = e.id
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	newName := m.availableLogNameLocked(e.label, e.id, old)
	if newName == old {
		return
	}
	oldPath := filepath.Join(m.logDir, old+".log")
	newPath := filepath.Join(m.logDir, newName+".log")
	if err := moveLogBundle(oldPath, newPath); err != nil {
		m.log.Log(opslog.Warn, "child log rename deferred", opslog.F("id", e.id), opslog.F("detail", err))
		return
	}
	e.logName = newName
	if m.registry != nil {
		if err := m.registry.SaveEntry(m.rowLocked(e)); err != nil {
			e.logName = old
			if rollback := moveLogBundle(newPath, oldPath); rollback != nil {
				m.log.Log(opslog.Error, "child log rename rollback failed", opslog.F("id", e.id), opslog.F("detail", rollback))
			}
			m.log.Log(opslog.Warn, "child log rename deferred", opslog.F("id", e.id), opslog.F("detail", err))
			return
		}
	}
	if e.logGeneration != "" {
		e.logGeneration += "-log-name-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
}

// moveLogBundle renames a full log family as one best-effort transaction, with
// rollback on failure. Never overwrite an existing file and never move symlinks.
// A stopped collector must close its Windows file handle before this is called.
func moveLogBundle(oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	suffixes := []string{""}
	for i := 1; i <= LogArchiveKeep; i++ {
		suffixes = append(suffixes, "."+strconv.Itoa(i))
	}
	suffixes = append(suffixes, ".rotating")
	type move struct{ from, to string }
	var pending []move
	for _, suffix := range suffixes {
		from, to := oldPath+suffix, newPath+suffix
		info, err := os.Lstat(from)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular log: %s", from)
		}
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("log file already exists: %s", to)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		pending = append(pending, move{from, to})
	}
	var moved []move
	for _, item := range pending {
		if err := os.Rename(item.from, item.to); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = os.Rename(moved[i].to, moved[i].from)
			}
			return fmt.Errorf("rename %s: %w", item.from, err)
		}
		moved = append(moved, item)
	}
	return nil
}
