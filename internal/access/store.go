package access

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrConflict = errors.New("설정이 다른 요청에서 변경되었습니다. 새로고침 후 다시 적용하세요")

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func newID(prefix string) string { return prefix + hex.EncodeToString(randomBytes(12)) }
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func cloneState(s State) (State, error) {
	b, e := json.Marshal(s)
	if e != nil {
		return State{}, e
	}
	var n State
	e = json.Unmarshal(b, &n)
	return n, e
}

// New is called after migration and runner restore, but before binding HTTP.
func New(db *sql.DB, r Runner, next http.Handler, ui []byte, dbPath string) (*Manager, error) {
	m := &Manager{db: db, runner: r, next: next, ui: ui, authSlots: make(chan struct{}, 4), now: time.Now, verifyOS: verifyOS, verifyAdmin: verifyOSAdmin, resolveOS: resolveOS, setupPath: dbPath + ".setup-token"}
	var raw string
	err := db.QueryRow("SELECT revision,document FROM access_state WHERE id=1").Scan(&m.revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		m.state = defaultState()
		rawBytes, e := json.Marshal(m.state)
		if e != nil {
			return nil, e
		}
		_, err = db.Exec("INSERT INTO access_state(id,revision,document) VALUES(1,1,?)", string(rawBytes))
		m.revision = 1
	} else if err == nil {
		err = json.Unmarshal([]byte(raw), &m.state)
	}
	if err != nil {
		return nil, fmt.Errorf("load access policy: %w", err)
	}
	if m.state.Processes == nil || len(m.state.ProcessGroups) == 0 {
		return nil, errors.New("invalid access policy; refusing an insecure reset")
	}
	// A corrupt policy must never become an unauthenticated installation.
	if err = validateState(&m.state); err != nil {
		return nil, fmt.Errorf("invalid access policy: %w", err)
	}
	if len(m.state.Users) == 0 {
		if err = m.loadSetupToken(); err != nil {
			// OS-admin bootstrap remains usable if the optional token file cannot be created.
			m.setupError = err.Error()
		}
	}
	nextState, _ := cloneState(m.state)
	changed := false
	if r != nil {
		for _, p := range r.List() {
			if _, ok := nextState.Processes[p.ID]; !ok {
				nextState.Processes[p.ID] = ProcessMeta{PG: "pg-default", Overrides: map[string]Grant{}, Protected: p.RunAs != nil}
				changed = true
			}
		}
	}
	if changed {
		if err = m.commit(nextState, "system", "프로세스 이관", "기존 프로세스", "관리자 전용으로 이관"); err != nil {
			return nil, err
		}
	}
	_, err = db.Exec("DELETE FROM access_session WHERE expires_at<?", m.now().Unix())
	return m, err
}
func (m *Manager) loadSetupToken() error {
	token := strings.TrimSpace(os.Getenv("SPAWNPOINT_SETUP_TOKEN"))
	if token != "" {
		if len(token) < 32 || len(token) > 256 {
			return errors.New("SPAWNPOINT_SETUP_TOKEN must contain 32 to 256 characters")
		}
		m.setupToken = token
		return nil
	}
	st, err := os.Lstat(m.setupPath)
	if err == nil {
		if !st.Mode().IsRegular() || st.Size() > 256 {
			return errors.New("invalid setup-token file")
		}
		f, e := openSetupToken(m.setupPath, false)
		if e != nil {
			return e
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 257))
		if e != nil {
			return e
		}
		token = strings.TrimSpace(string(b))
		if len(token) < 32 {
			return errors.New("invalid setup-token contents")
		}
		m.setupToken = token
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	token, err = randomToken()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(m.setupPath), 0700); err != nil {
		return err
	}
	f, err := openSetupToken(m.setupPath, true)
	if err != nil {
		return err
	}
	_, e := f.WriteString(token + "\n")
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	m.setupToken = token
	return nil
}

// Caller holds m.mu for writes. Compare-and-swap rejects stale policy snapshots.
// Policy changes and their audit entry commit in the same SQLite transaction.
func (m *Manager) commit(s State, actor, action, target, detail string) error {
	if err := validateState(&s); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE access_state SET revision=revision+1,document=? WHERE id=1 AND revision=?", string(raw), m.revision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if _, err = tx.Exec("INSERT INTO access_audit(at,actor,action,target,result,detail) VALUES(?,?,?,?,?,?)", m.now().UTC().Format(time.RFC3339), actor, action, target, "success", detail); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	m.state = s
	m.revision++
	return nil
}
func (m *Manager) audit(actor, action, target, result, detail string) error {
	_, e := m.db.Exec("INSERT INTO access_audit(at,actor,action,target,result,detail) VALUES(?,?,?,?,?,?)", m.now().UTC().Format(time.RFC3339), actor, action, target, result, detail)
	return e
}
func (m *Manager) audits() ([]map[string]any, error) {
	rows, e := m.db.Query("SELECT id,at,actor,action,target,result,detail FROM access_audit ORDER BY id DESC LIMIT 500")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int
		var at, actor, action, target, result, detail string
		if e = rows.Scan(&id, &at, &actor, &action, &target, &result, &detail); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"id": fmt.Sprint(id), "time": at, "actor": actor, "action": action, "object": target, "result": result, "detail": detail})
	}
	return out, rows.Err()
}
