package access

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type UserInput struct {
	ID       string   `json:"id"`
	Login    string   `json:"login"`
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	Groups   []string `json:"groups"`
	Auth     string   `json:"auth"`
	Status   string   `json:"status"`
	External string   `json:"external"`
	Password string   `json:"password"`
}
type AdminInput struct {
	Action       string           `json:"action"`
	Revision     int64            `json:"revision"`
	ID           string           `json:"id"`
	User         *UserInput       `json:"user"`
	Name         string           `json:"name"`
	Desc         string           `json:"desc"`
	Members      []string         `json:"members"`
	Guest        bool             `json:"guest"`
	GuestLogs    bool             `json:"guest_logs"`
	Rules        map[string]Grant `json:"rules"`
	Target       string           `json:"target"`
	Settings     *Settings        `json:"settings"`
	PublicGroups []string         `json:"public_groups"`
	IDs          []string         `json:"ids"`
	Destination  string           `json:"destination"`
	Clear        bool             `json:"clear"`
	Protected    *bool            `json:"protected"`
	Public       *bool            `json:"public"`
	Owner        string           `json:"owner"`
	Scope        string           `json:"scope"`
	Level        int              `json:"level"`
	Create       bool             `json:"create"`
	Expiry       string           `json:"expiry"`
}

func textOK(s string, max int) bool {
	return utf8.ValidString(s) && len(s) <= max && !strings.ContainsAny(s, "\x00\r\n")
}
func validateState(s *State) error {
	if s.Processes == nil || len(s.ProcessGroups) == 0 {
		return errors.New("프로세스 그룹이 필요합니다")
	}
	if !s.Settings.LocalLogin && !s.Settings.OSLogin {
		return errors.New("로그인 방법을 하나 이상 활성화하세요")
	}
	if s.Settings.OSAuto {
		return errors.New("통합 자동 인증은 아직 설정되지 않았습니다. OS 계정 ID/비밀번호 로그인을 사용하세요")
	}
	if !s.Settings.Lockout {
		return errors.New("로그인 시도 제한은 해제할 수 없습니다")
	}
	if !contains([]string{"15", "30", "60", "120"}, s.Settings.Session) {
		return errors.New("세션 만료는 15, 30, 60, 120분 중 선택하세요")
	}
	userIDs := map[string]bool{}
	logins := map[string]bool{}
	subjects := map[string]bool{}
	groups := map[string]bool{}
	names := map[string]bool{}
	for _, g := range s.UserGroups {
		if g.ID == "" || groups[g.ID] || strings.TrimSpace(g.Name) == "" || !textOK(g.Name, 128) || !textOK(g.Desc, 1024) || names[g.Name] {
			return errors.New("사용자 그룹 ID 또는 이름을 확인하세요")
		}
		groups[g.ID] = true
		names[g.Name] = true
	}
	usableAdmin := false
	for _, u := range s.Users {
		if u.ID == "" || userIDs[u.ID] || strings.TrimSpace(u.Login) == "" || strings.TrimSpace(u.Name) == "" || !textOK(u.Login, 128) || !textOK(u.Name, 128) || strings.ContainsAny(u.Login, " \t") || logins[strings.ToLower(u.Login)] || !validRole(u.Role) || !contains([]string{"active", "paused"}, u.Status) || u.Version < 1 {
			return errors.New("사용자 ID, 이름, 역할 또는 상태를 확인하세요")
		}
		userIDs[u.ID] = true
		logins[strings.ToLower(u.Login)] = true
		if u.Auth == "local" {
			if !strings.HasPrefix(u.Hash, "pbkdf2-sha256$") || u.Subject != "" || u.External != "" {
				return errors.New("자체 계정 인증 정보를 확인하세요")
			}
		} else if u.Auth == "os" {
			if u.Subject == "" || u.External == "" || u.Hash != "" || subjects[u.Subject] {
				return errors.New("OS 계정 연결이 없거나 중복되었습니다")
			}
			subjects[u.Subject] = true
		} else {
			return errors.New("OS 또는 자체 계정을 선택하세요")
		}
		seen := map[string]bool{}
		for _, g := range u.Groups {
			if !groups[g] || seen[g] {
				return errors.New("사용자 그룹을 확인하세요")
			}
			seen[g] = true
		}
		if u.Role == "admin" && u.Status == "active" && (u.Auth == "local" && s.Settings.LocalLogin || u.Auth == "os" && s.Settings.OSLogin) {
			usableAdmin = true
		}
	}
	if len(s.Users) > 0 && !usableAdmin {
		return errors.New("로그인 가능한 마지막 관리자는 정지·강등하거나 인증을 끌 수 없습니다")
	}
	validateRules := func(rs map[string]Grant, process bool) error {
		for key, r := range rs {
			known := strings.HasPrefix(key, "u:") && userIDs[strings.TrimPrefix(key, "u:")] || strings.HasPrefix(key, "g:") && groups[strings.TrimPrefix(key, "g:")]
			if !known || r.Level < 0 || r.Level > 3 || r.Create && (process || r.Level == 0) {
				return errors.New("권한 대상 또는 허용 범위를 확인하세요")
			}
		}
		return nil
	}
	pgs := map[string]bool{}
	names = map[string]bool{}
	for _, g := range s.ProcessGroups {
		if g.ID == "" || pgs[g.ID] || strings.TrimSpace(g.Name) == "" || !textOK(g.Name, 128) || !textOK(g.Desc, 1024) || names[g.Name] {
			return errors.New("프로세스 그룹 ID 또는 이름을 확인하세요")
		}
		pgs[g.ID] = true
		names[g.Name] = true
		if e := validateRules(g.Rules, false); e != nil {
			return e
		}
	}
	if !pgs["pg-default"] {
		return errors.New("기본 프로세스 그룹은 삭제할 수 없습니다")
	}
	for _, p := range s.Processes {
		if !pgs[p.PG] {
			return errors.New("프로세스 소속 그룹이 없습니다")
		}
		if e := validateRules(p.Overrides, true); e != nil {
			return e
		}
	}
	for _, t := range s.Tokens {
		if t.ID == "" || !userIDs[t.Owner] || !pgs[t.Scope] || t.Hash == "" || t.Level < 1 || t.Level > 3 || !textOK(t.Name, 128) {
			return errors.New("API 토큰 범위를 확인하세요")
		}
		if _, e := time.Parse("2006-01-02", t.Expiry); e != nil {
			return errors.New("토큰 만료일을 확인하세요")
		}
	}
	return nil
}
func (m *Manager) adminAction(w http.ResponseWriter, r *http.Request, p Principal, preview bool) {
	if r.Method != "POST" {
		fail(w, 405, "POST 요청이 필요합니다")
		return
	}
	var in AdminInput
	if e := readJSON(w, r, &in); e != nil {
		fail(w, 400, e.Error())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.currentPrincipal(p)
	if !admin(u) || p.Token != nil {
		fail(w, 403, "관리자 세션이 필요합니다")
		return
	}
	if m.state.Settings.Reauth && m.now().Unix()-p.AuthAt > 600 {
		send(w, 403, map[string]any{"ok": false, "error": map[string]string{"code": "reauth_required", "message": "민감한 변경을 위해 비밀번호를 다시 확인하세요"}})
		return
	}
	if in.Revision != m.revision {
		fail(w, 409, ErrConflict.Error())
		return
	}
	if preview && in.Action != "policy" && in.Action != "bulk" {
		fail(w, 400, "권한 또는 그룹 이동만 미리 볼 수 있습니다")
		return
	}
	next, _ := cloneState(m.state)
	secret := ""
	action, target, detail, e := m.applyAdmin(&next, in, &secret)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if e = validateState(&next); e != nil {
		fail(w, 400, e.Error())
		return
	}
	if preview {
		send(w, 200, map[string]any{"ok": true, "revision": m.revision, "impact": m.impact(&m.state, &next)})
		return
	}
	if e = m.commit(next, u.Login, action, target, detail); e != nil {
		status := 503
		if errors.Is(e, ErrConflict) {
			status = 409
		}
		fail(w, status, e.Error())
		return
	}
	send(w, 200, map[string]any{"ok": true, "revision": m.revision, "secret": secret})
}
func (m *Manager) applyAdmin(s *State, in AdminInput, secret *string) (action, target, detail string, err error) {
	bad := func(msg string) (string, string, string, error) { return "", "", "", errors.New(msg) }
	switch in.Action {
	case "user.upsert":
		if in.User == nil {
			return bad("사용자 정보가 필요합니다")
		}
		v := *in.User
		v.Login = strings.TrimSpace(v.Login)
		v.Name = strings.TrimSpace(v.Name)
		old := s.user(v.ID)
		if v.ID != "" && old == nil {
			return bad("사용자가 없습니다")
		}
		u := User{ID: v.ID, Login: v.Login, Name: v.Name, Role: v.Role, Groups: v.Groups, Auth: v.Auth, Status: v.Status, External: strings.TrimSpace(v.External), Version: 1, Last: "—"}
		if u.Groups == nil {
			u.Groups = []string{}
		}
		if u.ID == "" {
			u.ID = newID("u-")
		}
		if old != nil {
			u.Version = old.Version
			u.Last = old.Last
		}
		if u.Auth == "local" {
			u.External = ""
			if v.Password != "" {
				u.Hash, err = hashPassword(v.Password)
				if err != nil {
					return "", "", "", err
				}
			} else if old != nil && old.Auth == "local" {
				u.Hash = old.Hash
			} else {
				return bad("초기 비밀번호를 입력하세요")
			}
		} else if u.Auth == "os" {
			u.Subject, err = m.resolveOS(u.External)
			if err != nil {
				return bad("서버에서 확인 가능한 OS 계정을 입력하세요")
			}
		}
		if old != nil {
			if old.Role != u.Role || old.Status != u.Status || old.Auth != u.Auth || old.Subject != u.Subject || old.Login != u.Login || old.Hash != u.Hash {
				u.Version++
			}
			*old = u
		} else {
			s.Users = append(s.Users, u)
		}
		return "사용자 저장", u.Login, "역할·인증 변경 시 기존 세션 및 토큰 무효화", nil
	case "user.delete":
		if s.user(in.ID) == nil {
			return bad("사용자가 없습니다")
		}
		users := []User{}
		for _, u := range s.Users {
			if u.ID != in.ID {
				users = append(users, u)
			}
		}
		if len(users) == 0 {
			return bad("마지막 관리자는 삭제할 수 없습니다")
		}
		s.Users = users
		for i := range s.ProcessGroups {
			delete(s.ProcessGroups[i].Rules, "u:"+in.ID)
		}
		for id, p := range s.Processes {
			delete(p.Overrides, "u:"+in.ID)
			s.Processes[id] = p
		}
		tokens := []Token{}
		for _, t := range s.Tokens {
			if t.Owner != in.ID {
				tokens = append(tokens, t)
			}
		}
		s.Tokens = tokens
		return "사용자 삭제", in.ID, "계정 및 토큰 연결 제거", nil
	case "user-group.upsert":
		id := in.ID
		if id == "" {
			id = newID("g-")
		}
		g := UserGroup{ID: id, Name: strings.TrimSpace(in.Name), Desc: in.Desc, Icon: "users"}
		found := false
		for i := range s.UserGroups {
			if s.UserGroups[i].ID == id {
				s.UserGroups[i] = g
				found = true
			}
		}
		if in.ID != "" && !found {
			return bad("사용자 그룹이 없습니다")
		}
		if !found {
			s.UserGroups = append(s.UserGroups, g)
		}
		for _, id := range in.Members {
			if s.user(id) == nil {
				return bad("존재하지 않는 그룹 멤버입니다")
			}
		}
		for i := range s.Users {
			u := &s.Users[i]
			groups := []string{}
			for _, gid := range u.Groups {
				if gid != id {
					groups = append(groups, gid)
				}
			}
			if contains(in.Members, u.ID) {
				groups = append(groups, id)
			}
			u.Groups = groups
		}
		return "사용자 그룹 변경", g.Name, fmt.Sprintf("멤버 %d명", len(in.Members)), nil
	case "user-group.delete":
		gs := []UserGroup{}
		for _, g := range s.UserGroups {
			if g.ID != in.ID {
				gs = append(gs, g)
			}
		}
		if len(gs) == len(s.UserGroups) {
			return bad("그룹이 없습니다")
		}
		s.UserGroups = gs
		for i := range s.Users {
			g := []string{}
			for _, id := range s.Users[i].Groups {
				if id != in.ID {
					g = append(g, id)
				}
			}
			s.Users[i].Groups = g
		}
		for i := range s.ProcessGroups {
			delete(s.ProcessGroups[i].Rules, "g:"+in.ID)
		}
		for id, p := range s.Processes {
			delete(p.Overrides, "g:"+in.ID)
			s.Processes[id] = p
		}
		return "사용자 그룹 삭제", in.ID, "상속 규칙 제거", nil
	case "process-group.upsert":
		id := in.ID
		if id == "" {
			id = newID("pg-")
		}
		g := s.group(id)
		if g == nil {
			if in.ID != "" {
				return bad("그룹이 없습니다")
			}
			s.ProcessGroups = append(s.ProcessGroups, ProcessGroup{ID: id, Rules: map[string]Grant{}})
			g = s.group(id)
		}
		g.Name = strings.TrimSpace(in.Name)
		g.Desc = in.Desc
		g.Guest = in.Guest
		return "프로세스 그룹 저장", g.Name, "기존 권한 규칙 유지", nil
	case "process-group.delete":
		if in.ID == "pg-default" {
			return bad("기본 그룹은 삭제할 수 없습니다")
		}
		for _, p := range m.runner.List() {
			if s.meta(p.ID).PG == in.ID {
				return bad("프로세스를 다른 그룹으로 이동한 뒤 삭제하세요")
			}
		}
		gs := []ProcessGroup{}
		for _, g := range s.ProcessGroups {
			if g.ID != in.ID {
				gs = append(gs, g)
			}
		}
		if len(gs) == len(s.ProcessGroups) {
			return bad("그룹이 없습니다")
		}
		s.ProcessGroups = gs
		for id, p := range s.Processes {
			if p.PG == in.ID {
				delete(s.Processes, id)
			}
		}
		tokens := []Token{}
		for _, t := range s.Tokens {
			if t.Scope != in.ID {
				tokens = append(tokens, t)
			}
		}
		s.Tokens = tokens
		return "프로세스 그룹 삭제", in.ID, "범위 토큰 폐기", nil
	case "policy":
		if in.Rules == nil {
			in.Rules = map[string]Grant{}
		}
		if in.Target == "group" {
			g := s.group(in.ID)
			if g == nil {
				return bad("그룹이 없습니다")
			}
			g.Rules = in.Rules
		} else if in.Target == "process" {
			if _, ok := m.runner.Get(in.ID); !ok {
				return bad("프로세스가 없습니다")
			}
			p := s.meta(in.ID)
			p.Overrides = in.Rules
			s.Processes[in.ID] = p
		} else {
			return bad("권한 적용 대상을 확인하세요")
		}
		return "권한 변경", in.ID, "개별 예외는 해당 대상에만 적용", nil
	case "bulk":
		if len(in.IDs) == 0 || len(in.IDs) > 500 {
			return bad("1~500개 프로세스를 선택하세요")
		}
		if in.Destination != "" && s.group(in.Destination) == nil {
			return bad("이동할 그룹이 없습니다")
		}
		seen := map[string]bool{}
		for _, id := range in.IDs {
			if seen[id] {
				return bad("중복 프로세스입니다")
			}
			seen[id] = true
			if _, ok := m.runner.Get(id); !ok {
				return bad("프로세스가 변경되었습니다. 다시 확인하세요")
			}
			p := s.meta(id)
			if in.Destination != "" {
				p.PG = in.Destination
			}
			if in.Clear {
				p.Overrides = map[string]Grant{}
			}
			s.Processes[id] = p
		}
		return "프로세스 일괄 변경", fmt.Sprintf("%d개", len(in.IDs)), "그룹 이동·예외 정책 원자적 적용", nil
	case "process.meta":
		if _, ok := m.runner.Get(in.ID); !ok {
			return bad("프로세스가 없습니다")
		}
		p := s.meta(in.ID)
		if in.Protected != nil {
			p.Protected = *in.Protected
		}
		p.Guest = in.Public
		s.Processes[in.ID] = p
		return "프로세스 보호 설정", in.ID, "공개·편집 보호 변경", nil
	case "settings":
		if in.Settings == nil {
			return bad("설정이 필요합니다")
		}
		s.Settings = *in.Settings
		return "인증 정책 변경", "시스템", "인증·세션 정책 변경", nil
	case "guest":
		s.Settings.Guest = in.Guest
		s.Settings.GuestLogs = in.GuestLogs
		for _, id := range in.PublicGroups {
			if s.group(id) == nil {
				return bad("공개 대상 그룹이 없습니다")
			}
		}
		for i := range s.ProcessGroups {
			s.ProcessGroups[i].Guest = contains(in.PublicGroups, s.ProcessGroups[i].ID)
		}
		return "공개 범위 변경", "시스템", "비로그인 상태·로그 공개 범위 변경", nil
	case "token.create":
		u := s.user(in.Owner)
		if !active(u) {
			return bad("활성 사용자를 선택하세요")
		}
		expiry, e := time.Parse("2006-01-02", in.Expiry)
		if e != nil || expiry.Add(24*time.Hour).Before(m.now()) || expiry.After(m.now().Add(366*24*time.Hour)) {
			return bad("오늘부터 1년 이내의 만료일을 선택하세요")
		}
		value, e := randomToken()
		if e != nil {
			return "", "", "", e
		}
		*secret = "sp_" + value
		t := Token{ID: newID("t-"), Name: in.Name, Owner: u.ID, Version: u.Version, Scope: in.Scope, Level: in.Level, Create: in.Create, Expiry: in.Expiry, Last: "—", Hash: digest(*secret)}
		s.Tokens = append(s.Tokens, t)
		return "API 토큰 생성", t.ID, "사용자·그룹·최대 권한·만료일 제한", nil
	case "token.revoke":
		ts := []Token{}
		for _, t := range s.Tokens {
			if t.ID != in.ID {
				ts = append(ts, t)
			}
		}
		if len(ts) == len(s.Tokens) {
			return bad("토큰이 없습니다")
		}
		s.Tokens = ts
		return "API 토큰 폐기", in.ID, "즉시 무효화", nil
	}
	return bad("알 수 없는 관리 작업입니다")
}
func (m *Manager) impact(before, after *State) map[string]any {
	rows := []map[string]any{}
	gained, lost, changedRows := 0, 0, 0
	for _, p := range m.runner.List() {
		for _, u := range after.Users {
			old := evaluate(before, before.user(u.ID), p.ID, p.RunAs != nil)
			now := evaluate(after, &u, p.ID, p.RunAs != nil)
			up := ""
			down := ""
			names := []string{"조회", "로그", "실행", "편집", "생성"}
			a := []bool{old.View, old.Logs, old.Run, old.Edit, old.Create}
			b := []bool{now.View, now.Logs, now.Run, now.Edit, now.Create}
			for i := range a {
				if !a[i] && b[i] {
					up += names[i] + " "
					gained++
				}
				if a[i] && !b[i] {
					down += names[i] + " "
					lost++
				}
			}
			if up != "" || down != "" {
				changedRows++
			}
			if (up != "" || down != "") && len(rows) < 500 {
				rows = append(rows, map[string]any{"user": u.Login, "process": p.Label, "gained": strings.TrimSpace(up), "lost": strings.TrimSpace(down)})
			}
		}
	}
	return map[string]any{"gained": gained, "lost": lost, "rows": rows, "truncated": changedRows > 500, "revision": m.revision}
}
func (m *Manager) snapshot(p Principal) (map[string]any, error) {
	u := m.currentPrincipal(p)
	isAdmin := admin(u)
	users := []User{}
	groups := []UserGroup{}
	pgs := []map[string]any{}
	processes := []map[string]any{}
	tokens := []Token{}
	audit := []map[string]any{}
	for _, v := range m.state.Users {
		if isAdmin || u != nil && u.ID == v.ID {
			v.Hash = ""
			v.Subject = ""
			users = append(users, v)
		}
	}
	if isAdmin {
		groups = append(groups, m.state.UserGroups...)
		for _, t := range m.state.Tokens {
			t.Hash = ""
			tokens = append(tokens, t)
		}
		var e error
		audit, e = m.audits()
		if e != nil {
			return nil, e
		}
	} else if u != nil {
		for _, g := range m.state.UserGroups {
			if contains(u.Groups, g.ID) {
				groups = append(groups, g)
			}
		}
	}
	visibleGroups := map[string]bool{}
	for _, v := range m.runner.List() {
		cap := m.capabilities(p, v.ID, v.RunAs != nil)
		if !cap.View {
			continue
		}
		meta := m.state.meta(v.ID)
		visibleGroups[meta.PG] = true
		row := map[string]any{"id": v.ID, "name": v.Label, "pg": meta.PG, "status": v.Status, "pid": v.PID, "uptime": "—", "auto": v.AutoStart, "protected": meta.Protected || v.RunAs != nil, "overrides": map[string]Grant{}, "guest": meta.Guest, "env": map[string]string{}, "cmd": "", "cwd": "", "account": "Service account", "capabilities": cap, "error": ""}
		if v.Status == runnerRunning && v.StartedAt != nil {
			row["uptime"] = m.now().Sub(*v.StartedAt).Truncate(time.Second).String()
		}
		if u != nil {
			row["cmd"] = v.Cmd
			row["error"] = v.Error
			if v.Cwd != nil {
				row["cwd"] = *v.Cwd
			}
			if v.RunAs != nil {
				account := v.RunAs.Username
				if v.RunAs.Domain != "" {
					account = v.RunAs.Domain + "\\" + account
				}
				row["account"] = account
			}
			if cap.Edit || isAdmin {
				row["env"] = v.Env
			}
		}
		if isAdmin {
			row["overrides"] = meta.Overrides
		}
		if u == nil {
			row = map[string]any{"id": v.ID, "name": v.Label, "pg": meta.PG, "status": v.Status, "capabilities": cap}
		}
		processes = append(processes, row)
	}
	for _, g := range m.state.ProcessGroups {
		create := m.mayCreate(p, g.ID)
		if !isAdmin && !visibleGroups[g.ID] && !create {
			continue
		}
		row := map[string]any{"id": g.ID, "name": g.Name, "desc": "", "rules": map[string]Grant{}, "guest": g.Guest, "can_create": create}
		if u != nil {
			row["desc"] = g.Desc
		}
		if isAdmin {
			row["rules"] = g.Rules
		}
		pgs = append(pgs, row)
	}
	settings := m.state.Settings
	return map[string]any{"ok": true, "revision": m.revision, "actor": p.UserID, "db": map[string]any{"users": users, "userGroups": groups, "processGroups": pgs, "processes": processes, "settings": settings, "tokens": tokens, "audit": audit}}, nil
}

const runnerRunning = "running"
