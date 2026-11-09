package access

// A per-user rule takes precedence over group membership rules, including an
// explicit per-user deny. Within group membership rules, explicit deny wins.
// Process exceptions replace only the named subject's inherited rule.
func groupGrant(u *User, g *ProcessGroup, overrides map[string]Grant) (Grant, string) {
	if !active(u) || g == nil {
		return Grant{}, "기본 거부"
	}
	if admin(u) {
		return Grant{3, true}, "관리자 전체 권한"
	}
	rules := map[string]Grant{}
	for k, v := range g.Rules {
		rules[k] = v
	}
	for k, v := range overrides {
		rules[k] = v
	}
	var out Grant
	source := "기본 거부 · 허용 규칙 없음"
	if r, ok := rules["u:"+u.ID]; ok {
		out = r
		source = "사용자 직접 설정"
	} else {
		for _, id := range u.Groups {
			if r, ok := rules["g:"+id]; ok {
				if r.Level == 0 {
					return Grant{}, "사용자 그룹의 명시적 거부"
				}
				if r.Level > out.Level {
					out.Level = r.Level
				}
				out.Create = out.Create || r.Create
				source = "사용자 그룹 상속"
			}
		}
	}
	if u.Role == "viewer" && out.Level > 1 {
		out.Level = 1
	}
	out.Create = out.Create && out.Level > 0 && u.Role == "developer"
	return out, source
}
func evaluate(s *State, u *User, id string, specificAccount bool) Capabilities {
	p := s.meta(id)
	g := s.group(p.PG)
	if g == nil {
		return Capabilities{Source: "그룹 없음"}
	}
	if u == nil {
		public := g.Guest
		if p.Guest != nil {
			public = *p.Guest
		}
		v := s.Settings.Guest && public
		return Capabilities{View: v, Logs: v && s.Settings.GuestLogs, Source: "공개 상태 조회"}
	}
	if !active(u) {
		return Capabilities{Source: "정지된 계정"}
	}
	if admin(u) {
		return Capabilities{Level: 3, Raw: 3, View: true, Logs: true, Run: true, Edit: true, Create: true, Remove: true, Source: "관리자 전체 권한"}
	}
	grant, source := groupGrant(u, g, p.Overrides)
	base, _ := groupGrant(u, g, nil)
	// A saved execution credential is always protected, independent of the UI
	// checkbox. Editing a command is delegation of the execution account itself.
	return Capabilities{Level: grant.Level, Raw: grant.Level, View: grant.Level >= 1, Logs: grant.Level >= 1, Run: grant.Level >= 2, Edit: grant.Level >= 3 && !p.Protected && !specificAccount, Create: base.Create, Source: source}
}
func (m *Manager) capabilities(p Principal, id string, specific bool) Capabilities {
	c := evaluate(&m.state, m.state.user(p.UserID), id, specific)
	if p.Token != nil {
		t := p.Token
		if m.state.meta(id).PG != t.Scope {
			return Capabilities{Source: "토큰 범위 밖"}
		}
		c.Level = min(c.Level, t.Level)
		c.View = c.View && t.Level >= 1
		c.Logs = c.Logs && t.Level >= 1
		c.Run = c.Run && t.Level >= 2
		c.Edit = c.Edit && t.Level >= 3 && !specific && !m.state.meta(id).Protected
		c.Create = c.Create && t.Create
		c.Remove = false
	}
	return c
}
func (m *Manager) mayCreate(p Principal, gid string) bool {
	u := m.state.user(p.UserID)
	g := m.state.group(gid)
	if !active(u) || g == nil {
		return false
	}
	grant, _ := groupGrant(u, g, nil)
	yes := admin(u) || grant.Create
	if p.Token != nil {
		yes = yes && p.Token.Scope == gid && p.Token.Create && p.Token.Level >= 3 && (u.Role == "developer" || u.Role == "admin")
	}
	return yes
}
