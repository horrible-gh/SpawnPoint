package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"spawnpoint/internal/runner"
)

// Field limits (0008-L 1.2). Both are counts of code points, not bytes: the
// database's CHECK constraints count characters, so a byte-based limit here
// would accept a label the store then rejects.
const (
	cmdMaxLen   = 4096
	labelMaxLen = 128
)

// processJSON is the `process` block, in the field order 0007-P uses. The order
// is not decorative — the contract tests compare whole response bodies, which
// is the only way to notice a field that quietly stopped being sent.
type processJSON struct {
	ID    string            `json:"id"`
	Label string            `json:"label"`
	Cmd   string            `json:"cmd"`
	Cwd   *string           `json:"cwd"`
	Env   map[string]string `json:"env"`
	// PID is the shell's process, not the program the user named: the program
	// is that shell's descendant (0004-NR 3.3). Kept as-is from the current
	// contract.
	PID       *int    `json:"pid"`
	Status    string  `json:"status"`
	ExitCode  *int    `json:"exit_code"`
	StartedAt *string `json:"started_at"`
	EndedAt   *string `json:"ended_at"`
	// Error is only carried when Status is failed (0008-L 4.5). omitempty on a
	// string is exactly that rule, because a failed start always has a reason
	// and nothing else ever sets one.
	Error string `json:"error,omitempty"`
}

type listResponse struct {
	OK        bool          `json:"ok"`
	Processes []processJSON `json:"processes"`
}

// writeResponse carries `persisted`, which registration and update send and
// nothing else does. It is always present, never omitted: the whole point of
// the field is that a caller can read the save's outcome without guessing, and
// an absent key would be indistinguishable from an old server (0007-P
// [새 명령 등록 — 저장 실패]).
type writeResponse struct {
	OK        bool        `json:"ok"`
	Persisted bool        `json:"persisted"`
	Process   processJSON `json:"process"`
}

type processResponse struct {
	OK      bool        `json:"ok"`
	Process processJSON `json:"process"`
}

type deleteResponse struct {
	OK        bool   `json:"ok"`
	DeletedID string `json:"deleted_id"`
}

// toJSON renders one entry.
func toJSON(info runner.Info) processJSON {
	env := info.Env
	if env == nil {
		// `{}`, never null. The screen iterates it without checking.
		env = map[string]string{}
	}
	out := processJSON{
		ID:        info.ID,
		Label:     info.Label,
		Cmd:       info.Cmd,
		Cwd:       info.Cwd,
		Env:       env,
		PID:       info.PID,
		Status:    info.Status,
		ExitCode:  info.ExitCode,
		StartedAt: responseTimePtr(info.StartedAt),
		EndedAt:   responseTimePtr(info.EndedAt),
	}
	if info.Status == runner.StatusFailed {
		out.Error = info.Error
	}
	return out
}

// --- GET /processes ----------------------------------------------------------

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) {
	// A JSON array is never null on the wire: the screen renders
	// data.processes.length and a null would be an empty page plus a console
	// error rather than an empty list.
	out := []processJSON{}
	if s.runner != nil {
		for _, info := range s.runner.List() {
			out = append(out, toJSON(info))
		}
	}
	s.writeJSON(w, r, http.StatusOK, listResponse{OK: true, Processes: out})
}

// --- POST /processes , PUT /processes/<id> -----------------------------------

// registration is a validated request body.
type registration struct {
	label string
	cmd   string
	cwd   *string
	env   map[string]string
}

// validateRegistration is 0008-L 4.1's field order: cmd, label, cwd, env. It
// returns at the first failure and never accumulates — a caller fixing one
// field at a time is the contract, not a limitation.
func validateRegistration(obj map[string]json.RawMessage) (registration, *apiError) {
	var out registration

	raw, ok := present(obj, "cmd")
	if !ok {
		return out, ptr(invalidField("cmd", "cmd cannot be empty."))
	}
	cmd, ok := asString(raw)
	if !ok || strings.TrimSpace(cmd) == "" {
		return out, ptr(invalidField("cmd", "cmd cannot be empty."))
	}
	if utf8.RuneCountInString(cmd) > cmdMaxLen {
		return out, ptr(invalidField("cmd", "cmd is too long."))
	}
	out.cmd = cmd

	if raw, ok := present(obj, "label"); !ok {
		// No display name given, so the command names itself. Validation has
		// already established that cmd has a non-blank first word, so this
		// cannot come back empty (E-31).
		out.label = firstWord(cmd)
	} else {
		label, ok := asString(raw)
		if !ok || strings.TrimSpace(label) == "" {
			return out, ptr(invalidField("label", "label must be a string."))
		}
		if utf8.RuneCountInString(label) > labelMaxLen {
			return out, ptr(invalidField("label", "label is too long."))
		}
		out.label = label
	}

	if raw, ok := present(obj, "cwd"); ok {
		cwd, ok := asString(raw)
		if !ok {
			return out, ptr(invalidField("cwd", "cwd must be a string."))
		}
		// An empty working directory means "not set", the same as omitting it:
		// the two are indistinguishable to a user and only one of them can be
		// checked for existence (0007-P [새 명령 등록 — label 생략]).
		if cwd != "" {
			out.cwd = &cwd
		}
	}

	out.env = map[string]string{}
	if raw, ok := present(obj, "env"); ok {
		env, ok := asStringMap(raw)
		if !ok {
			return out, ptr(invalidField("env", "env must be an object with string keys and values."))
		}
		out.env = env
	}
	return out, nil
}

// firstWord is the display name derived from a command: everything up to the
// first run of whitespace.
func firstWord(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return cmd
	}
	return fields[0]
}

func (s *Server) serveRegister(w http.ResponseWriter, r *http.Request) {
	obj, apiErr := readObject(r)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	reg, apiErr := validateRegistration(obj)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	if s.runner == nil {
		s.fail(w, r, notFoundError(msgUnknownEndpoint))
		return
	}

	// Registering and starting is one call because the order between them is
	// the contract: the row is written first, so a command that cannot run is
	// still on the list afterwards and can be corrected (0007-P
	// [새 명령 등록 — 정상], 0004-NR F6). A start that fails is still a 200 —
	// the registration succeeded, and the failure is carried by `status` and
	// `error` instead.
	info, persisted := s.runner.Register(reg.label, reg.cmd, reg.cwd, reg.env)
	s.writeJSON(w, r, http.StatusOK, writeResponse{OK: true, Persisted: persisted, Process: toJSON(info)})
}

// serveUpdate replaces a registration and leaves the process running.
//
// The screen's "Save & Restart" is this request followed by a separate restart.
// The server does not fuse the two: a caller that only wanted to fix a typo in
// the label would otherwise have its service bounced (0007-P [등록 정보 수정]).
func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request, id string) {
	obj, apiErr := readObject(r)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	reg, apiErr := validateRegistration(obj)
	if apiErr != nil {
		s.fail(w, r, *apiErr)
		return
	}
	if s.runner == nil {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	// PUT replaces, so every field is sent, including the ones the body left
	// out — those were resolved to their defaults by validation. A caller that
	// omits `env` is asking for no extra environment, not for the old one.
	cwd := reg.cwd
	info, persisted, ok := s.runner.Update(id, &reg.label, &reg.cmd, &cwd, reg.env)
	if !ok {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	s.writeJSON(w, r, http.StatusOK, writeResponse{OK: true, Persisted: persisted, Process: toJSON(info)})
}

// --- POST /processes/<id>/run|stop|restart -----------------------------------

// serveAction runs one of the three lifecycle operations.
//
// None of them is an error when it has nothing to do: run on a running entry
// and stop on a stopped one both answer 200 with the current state (E-16,
// E-17). A double click is not a fault, and reporting it as one would train
// people to ignore the error banner.
func (s *Server) serveAction(w http.ResponseWriter, r *http.Request, id, action string) {
	if s.runner == nil {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	var (
		info runner.Info
		ok   bool
	)
	switch action {
	case "run":
		info, ok = s.runner.Run(id)
	case "stop":
		info, ok = s.runner.Stop(id)
	case "restart":
		info, ok = s.runner.Restart(id)
	}
	if !ok {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	s.writeJSON(w, r, http.StatusOK, processResponse{OK: true, Process: toJSON(info)})
}

// --- DELETE /processes/<id> --------------------------------------------------

// serveDelete removes the registration, the process and the logs.
//
// The reply has no `process` block. There is no longer a process to describe,
// and sending its last known state would invite a caller to keep showing a row
// for something that has been deleted (0007-P [삭제]).
func (s *Server) serveDelete(w http.ResponseWriter, r *http.Request, id string) {
	if s.runner == nil || !s.runner.Delete(id) {
		s.fail(w, r, notFoundError(msgProcessNotFound))
		return
	}
	s.writeJSON(w, r, http.StatusOK, deleteResponse{OK: true, DeletedID: id})
}

func ptr(e apiError) *apiError { return &e }
