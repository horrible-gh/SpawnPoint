package access

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func applyTestUser(t *testing.T, m *Manager, s *State, in UserInput) error {
	t.Helper()
	next, err := cloneState(*s)
	if err != nil {
		t.Fatal(err)
	}
	var secret string
	_, _, _, err = m.applyAdmin(&next, AdminInput{Action: "user.upsert", User: &in}, &secret)
	if err == nil {
		err = validateState(&next)
	}
	if err == nil {
		*s = next
	}
	return err
}
func osUserInput(external string) UserInput {
	return UserInput{Name: "OS account", Role: "viewer", Auth: "os", Status: "active", External: external, Groups: []string{"dev"}}
}
func TestOSUserNeedsNoSeparateLogin(t *testing.T) {
	for _, tc := range []struct{ external, subject string }{
		{`.\serveruser`, "windows:sid:S-1-5-21-111-222-333-1001"},
		{`DOMAIN\Server User`, "windows:sid:S-1-5-21-111-222-333-1002"},
		{"person@example.test", "windows:sid:S-1-5-21-111-222-333-1003"},
		{"linuxuser", "linux:uid:1001"},
	} {
		t.Run(tc.external, func(t *testing.T) {
			s := fixturePolicy()
			m := &Manager{resolveOS: func(name string) (string, error) {
				if name != tc.external {
					t.Fatalf("wrong external account %q", name)
				}
				return tc.subject, nil
			}}
			in := osUserInput(tc.external)
			// Neither login nor an internal ID is supplied by the administrator.
			if e := applyTestUser(t, m, &s, in); e != nil {
				t.Fatal(e)
			}
			u := s.Users[len(s.Users)-1]
			if u.ID == "" || u.ID == tc.external || u.Subject != tc.subject || u.Login != tc.external || u.Hash != "" || u.Version != 1 {
				t.Fatalf("bad OS identity: %+v", u)
			}
			in.ID = u.ID
			in.Name = "Renamed OS account"
			in.Login = "must-not-rebind"
			in.Password = "not an OS credential"
			if e := applyTestUser(t, m, &s, in); e != nil {
				t.Fatal(e)
			}
			after := s.user(u.ID)
			if after.ID != u.ID || after.Login != u.Login || after.Subject != u.Subject || after.Version != u.Version || after.Hash != "" {
				t.Fatal("metadata edit changed authentication")
			}
			dup := osUserInput(tc.external)
			dup.Login = "different-alias"
			if e := applyTestUser(t, m, &s, dup); e == nil {
				t.Fatal("duplicate SID/UID accepted under another alias")
			}
		})
	}
}
func TestOSUserEditPreservesLegacyIdentityAndGrants(t *testing.T) {
	s := fixturePolicy()
	old := User{ID: "legacy-os", Login: "sp_admin", Name: "Legacy", Role: "developer", Auth: "os", External: `.\serveruser`, Subject: "windows:sid:legacy", Groups: []string{"dev"}, Status: "active", Version: 4, Last: "today"}
	s.Users = append(s.Users, old)
	s.ProcessGroups[0].Rules["u:legacy-os"] = Grant{Level: 3, Create: true}
	p := s.Processes["p"]
	p.Overrides["u:legacy-os"] = Grant{Level: 2}
	s.Processes["p"] = p
	m := &Manager{resolveOS: func(string) (string, error) { return old.Subject, nil }}
	in := osUserInput(old.External)
	in.ID = old.ID
	in.Role = old.Role
	in.Name = "Updated"
	if e := applyTestUser(t, m, &s, in); e != nil {
		t.Fatal(e)
	}
	u := s.user(old.ID)
	if u.Login != old.Login || u.Subject != old.Subject || u.Version != old.Version || u.Last != old.Last || len(u.Groups) != 1 || !evaluate(&s, u, "p", false).Run {
		t.Fatal("legacy identity, session version or permission changed")
	}
	in.Role = "viewer"
	if e := applyTestUser(t, m, &s, in); e != nil {
		t.Fatal(e)
	}
	if s.user(old.ID).Version != old.Version+1 {
		t.Fatal("role change did not invalidate sessions")
	}
	// Other identities cannot replace this entry through an alias-only update.
	m.resolveOS = func(string) (string, error) { return "windows:sid:replacement", nil }
	if e := applyTestUser(t, m, &s, in); e != nil {
		t.Fatal(e)
	}
	if s.user(old.ID).Version != old.Version+2 {
		t.Fatal("SID change did not invalidate sessions")
	}
}
func TestLocalAndOSLoginNamespacesRemainSeparate(t *testing.T) {
	s := fixturePolicy()
	m := &Manager{resolveOS: func(string) (string, error) { return "linux:uid:999", nil }}
	in := osUserInput("viewer") // Same text as an existing local login is not the same identity.
	if e := applyTestUser(t, m, &s, in); e != nil {
		t.Fatal(e)
	}
	osID := s.Users[len(s.Users)-1].ID
	m.state = s
	m.verifyOS = func(context.Context, string, string) (string, error) { return "linux:uid:999", nil }
	got, e := m.authenticate(context.Background(), "os", "viewer", "native password")
	if e != nil || got.ID != osID {
		t.Fatalf("OS login resolved to local identity: %v", e)
	}
	if _, e = m.authenticate(context.Background(), "local", "viewer", "native password"); e == nil {
		t.Fatal("OS identity enabled local authentication")
	}
	dup := UserInput{Login: "VIEWER", Name: "Duplicate", Role: "viewer", Auth: "local", Status: "active", Password: "long local password"}
	if e = applyTestUser(t, m, &s, dup); e == nil {
		t.Fatal("duplicate local login accepted")
	}
	dup.Login = ""
	if e = applyTestUser(t, m, &s, dup); e == nil {
		t.Fatal("local login no longer required")
	}
	// Changing OS -> local needs an explicit login and password, but keeps ID/grants.
	convert := osUserInput("viewer")
	convert.ID = osID
	convert.Auth = "local"
	if e = applyTestUser(t, m, &s, convert); e == nil {
		t.Fatal("conversion without password accepted")
	}
	convert.Password = "new local password"
	if e = applyTestUser(t, m, &s, convert); e == nil {
		t.Fatal("conversion without local login accepted")
	}
	convert.Login = "converted"
	if e = applyTestUser(t, m, &s, convert); e != nil {
		t.Fatal(e)
	}
	u := *s.user(osID)
	if u.ID != osID || u.Auth != "local" || u.Subject != "" || u.External != "" || u.Hash == "" || u.Version != 2 {
		t.Fatal("invalid local conversion")
	}
	back := osUserInput("viewer")
	back.ID = osID
	if e = applyTestUser(t, m, &s, back); e != nil {
		t.Fatal(e)
	}
	if s.user(osID).Hash != "" || s.user(osID).Login != "viewer" || s.user(osID).Version != 3 {
		t.Fatal("invalid OS conversion")
	}
}
func TestOSUserRejectsMissingUnknownAndMalformedIdentities(t *testing.T) {
	s := fixturePolicy()
	calls := 0
	m := &Manager{resolveOS: func(string) (string, error) { calls++; return "", errors.New("not found") }}
	for _, name := range []string{"", "  ", "bad\nname", strings.Repeat("x", 257)} {
		if e := applyTestUser(t, m, &s, osUserInput(name)); e == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if calls != 0 {
		t.Fatal("malformed identity reached OS resolver")
	}
	if e := applyTestUser(t, m, &s, osUserInput("unknown")); e == nil {
		t.Fatal("unknown account accepted")
	}
	if len(s.Users) != 4 {
		t.Fatal("failed update changed state")
	}
}
