package runner

import "spawnpoint/internal/opslog"

// AutoStart starts opted-in registrations once per manager lifetime. Call it
// only after Restore and a successful listener bind, before serving requests.
// This is startup policy, not supervision: a failed or later stopped command
// is not retried automatically. Run retains its normal duplicate-start guard.
func (m *Manager) AutoStart(stop <-chan struct{}) {
	m.autoStartOnce.Do(func() {
		// Shutdown waits on the same lock before taking its child snapshot.
		// Otherwise a stop arriving during spawn could miss the new child.
		m.autoStartMu.Lock()
		defer m.autoStartMu.Unlock()
		for _, info := range m.List() {
			select {
			case <-stop:
				return
			default:
			}
			if !info.AutoStart {
				continue
			}
			m.log.Log(opslog.Info, "automatic start", opslog.F("id", info.ID))
			// Run records a failed start on this entry and in the operations
			// log. A failure must not prevent the remaining entries from starting.
			m.Run(info.ID)
		}
	})
}
