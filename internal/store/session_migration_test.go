package store

import "testing"

func TestRememberedSessionMigrationPreservesExistingSessions(t *testing.T) {
	s := openTemp(t)
	if _, e := s.DB().Exec(migrationsDDL); e != nil {
		t.Fatal(e)
	}
	scripts, e := s.adapter.Migrations()
	if e != nil {
		t.Fatal(e)
	}
	for _, script := range scripts[:7] {
		if e := s.applyOne(script); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.DB().Exec("INSERT INTO access_session(digest,user_id,user_version,csrf,created_at,last_seen,expires_at,auth_at) VALUES('old-hash','admin',3,'csrf',100,110,43300,105)"); e != nil {
		t.Fatal(e)
	}
	already, applied, e := s.Migrate()
	if e != nil || already != 7 || applied != 1 {
		t.Fatalf("migration: %d %d %v", already, applied, e)
	}
	var hash, previous, user, csrf string
	var remembered, overlap, rotated, expires, auth, version, last int64
	e = s.DB().QueryRow("SELECT token_digest,previous_digest,user_id,csrf,remembered,previous_until,rotated_at,expires_at,auth_at,user_version,last_seen FROM access_session WHERE digest='old-hash'").Scan(&hash, &previous, &user, &csrf, &remembered, &overlap, &rotated, &expires, &auth, &version, &last)
	if e != nil {
		t.Fatal(e)
	}
	if hash != "old-hash" || previous != "" || user != "admin" || csrf != "csrf" || remembered != 0 || overlap != 0 || rotated != 100 || expires != 43300 || auth != 105 || version != 3 || last != 110 {
		t.Fatal("upgrade changed existing session or granted persistent login without consent")
	}
	if _, e = s.DB().Exec("UPDATE access_session SET remembered=2"); e == nil {
		t.Fatal("invalid remembered flag accepted")
	}
	if _, a, e := s.Migrate(); e != nil || a != 0 {
		t.Fatal("migration was reapplied")
	}
}
