# Remembered browser sessions

The login checkbox now persists the user's choice on the server, not just in
browser cookie attributes. It applies to both local and OS-account sign-in.

- Unchecked: browser-session cookie, configured 15/30/60/120-minute idle timeout,
  and a 12-hour absolute lifetime, unchanged.
- Checked: persistent HttpOnly/SameSite=Strict cookie (Secure on HTTPS), with a
  **30-day absolute lifetime measured from the password login**. Ordinary idle
  expiry does not sign this browser out. The deadline is not extended forever.
- After 12 hours, browser entry (`GET /auth/me`) or a valid user heartbeat rotates
  the credential. Background process/log polling does not renew it. Rotation
  keeps the original deadline, CSRF token and password verification time.
- Concurrent tabs use an atomic token compare-and-swap. The previous token has
  a 60-second overlap for in-flight requests and cannot mint another successor.
  Only token hashes are stored; the internal session row ID remains stable.
- Explicit logout deletes the row and clears the cookie. Other-session revocation,
  password/role/security-version changes, account suspension and disabled login
  methods still invalidate access. Sensitive administrator changes still require
  the existing password reauthentication after 10 minutes.
- Temporary session database failures return 503 rather than clearing cookies or
  treating the user as logged out. An invalid/stale request cannot clear a cookie
  that another tab has just renewed.

Migration `008_remembered_sessions.sql` preserves existing sessions and process
registrations. Older versions did not store the checkbox choice, so **log in once
with the checkbox selected after upgrading** to enable the new behavior. Existing
sessions are not silently promoted to long-lived sessions. A normal server restart
with the same database preserves remembered sessions, including rotated tokens.
These rules supersede the old blanket 12-hour/idle description in
`access-control.md` for explicitly remembered browser sessions only.

## Verification

`go test ./...` includes idle/absolute expiry, rotation, 24 simultaneous requests,
CSRF/origin/bearer boundaries, reauthentication age, logout, revocation, opt-out,
SQLite reopen and legacy migration preservation. Run concurrency checks with
`go test -race ./internal/access ./internal/store`.

`python tools/test-remembered-session-browser.py <built-server>` uses a disposable
SQLite database and a real persistent Chromium profile: it checks the actual login
checkbox, long-idle return, browser close/reopen, server restart, multi-tab renewal,
logout, opt-out and absolute expiry. It does not inject saved cookies or use a mock
HTTP bridge. `.github/workflows/verify-sessions.yml` runs Linux/Windows regression
and Linux real-browser verification.
