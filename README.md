# SpawnPoint

A **entry point** module that creates (spawns) new task instances. It receives external
requests to create new instances, validates them, registers qualified requests with unique
identifiers, and returns handles (identifiers + status) to callers.

Design document chain: D-0004 (architecture) → P-0005 (protocol) → L-0006 (logic) → DB-0007 (database).
This implementation translates the rules finalized in those four documents into code.

## Structure

```
app/
  main.py            Application entry point (read env settings → configure storage/auth → start server; serves API + UI + runner on one port)
runnerview/
  static/index.html  Runner UI (dynamic screen connected to actual /processes* API)
  page.py            Static screen loader
spawnpoint/
  params.py          Parameters (L-0006 §1)
  clock.py           Time/timezone utility (display=KST, storage/comparison=fixed-width UTC ISO)
  models.py          SpawnInstance model (DB-0007 §2.1)
  storage.py         Registry — SQLite-backed store using sqloader, insert/query/seq
  sql/
    sqlite/            sqloader query files (spawn_instance.json / spawn_daily_seq.json + .sql)
    migration/sqlite/  sqloader migration files (DB-0007 §2 DDL)
  validator.py       Validation layer (L-0006 §2.2)
  allocator.py       ID allocator (L-0006 §2.3)
  auth.py            Bearer token validator
  results.py         Response builder (P-0005 response format)
  service.py         Top-level handler spawn() (L-0006 §2.1)
  runner.py          Runner (ProcessManager) — actual OS subprocess run/stop/restart/list/logging
  http_api.py        HTTP adapter (POST /spawn + screen serving + /processes* runner routes)
tests/
  test_spawnpoint.py       Protocol 4 scenarios + boundary conditions (core unit tests)
  test_http_integration.py Actual socket boot + end-to-end validation (+ healthz/405/413/404)
  test_runnerview.py       Verify one server serves both screen (/) and API (/spawn)
  test_runner.py           ProcessManager unit tests (start/stop/restart/list/logging)
  test_processes_http.py   /processes* routes end-to-end validation
```

The API server and UI run on **one process, one port**. `app/main.py` injects the static
UI HTML from `runnerview` into `spawnpoint.http_api` to serve it on `GET /`.

Component flow (D-0004 §2): `Intake → Validator → Allocator → Registrar → Responder`.

## Endpoints

`POST /spawn` — `Content-Type: application/json`. In default local mode, no authentication
headers required. When `SPAWNPOINT_API_TOKENS` is set, `Authorization: Bearer <token>`
header is required.

Request:
```json
{
  "requester": "user-a1b2c3",
  "kind": "session",
  "request_key": "req-7f3a90",
  "options": { "label": "nightly-build", "ttl_seconds": 3600 }
}
```

Success response:
```json
{
  "ok": true,
  "instance": {
    "id": "spwn_20260725_0001a7c3f0",
    "status": "created",
    "kind": "session",
    "requester": "user-a1b2c3",
    "created_at": "2026-07-25T13:40:12+09:00"
  }
}
```

- Validation failure → `{"ok": false, "error": {"code": "invalid_request", "field": "...", "message": "..."}}` (HTTP 400)
- Auth failure → `code: "unauthorized"` (HTTP 401)
- Duplicate request (same `request_key` arrives within `dedup_window` [300 seconds]) → returns existing instance with `"deduplicated": true`
- Oversized body → `code: "payload_too_large"` (HTTP 413, limit 64KiB)
- Storage failure → `code: "storage_error"` (HTTP 500)

### Screen / Operational Routes

- `GET /`, `GET /index.html` → Runner UI (HTML). Same port as API.
- `GET /healthz` → `{"ok": true, "status": "healthy"}` (liveness check, no auth)
- Unknown path → HTTP 404 `not_found`
- Unsupported method (PUT/DELETE/PATCH) → HTTP 405 `method_not_allowed`

### Runner (Process Runner) Routes

In default local mode, no authentication required. When `SPAWNPOINT_API_TOKENS` is set,
all require `Authorization: Bearer <token>`. These are separate from spawn() instance
registration (above) — they manage actual OS subprocess lifecycle.

- `POST /processes` — Start new process. Body: `{"cmd": "...", "label": "...", "cwd": "...", "env": {"K":"V"}}`
  (`cmd` required, others optional. If `label` omitted, uses first token of `cmd`). Runs with `shell=True`
  and appends stdout+stderr to `logs/<id>.log`.
- `GET /processes` — List running/exited processes. Each item: `id, label, cmd, cwd, pid,
  status(running|exited|killed|stopped), exit_code, started_at, ended_at`.
- `POST /processes/<id>/stop` — Terminate process tree (POSIX: `killpg` SIGTERM→SIGKILL,
  Windows: `taskkill /PID <pid> /T /F`).
- `POST /processes/<id>/restart` — Stop if running, then restart with same id.
- `GET /processes/<id>/logs?offset=<bytes>` — Return new log text after `offset` and
  next `offset` (polling tail). UI polls every 1 second.
- Non-existent id → HTTP 404 `not_found`.

## ID Format

`spwn_{YYYYMMDD}_{seq}{rand}` — per-day sequence (atomic increment, 4-digit zero-padded) + 6-digit hex
random suffix. Uniqueness first guaranteed by (date, seq), then by PK constraint,
collision absorbed via retry path.

## Running

```bash
# Start server (default local mode: no auth)
python -m app.main
#   UI: http://127.0.0.1:8091/
#   API:      POST http://127.0.0.1:8091/spawn
# → UI and API run together on one process, one port. No need to start two apps.

# On deployment, set allowed tokens
SPAWNPOINT_API_TOKENS="tok-a,tok-b" SPAWNPOINT_DB_PATH=/var/lib/spawnpoint.db python -m app.main
```

Environment variables: `SPAWNPOINT_HOST` (127.0.0.1), `SPAWNPOINT_PORT` (8091),
`SPAWNPOINT_DB_PATH` (spawnpoint.db), `SPAWNPOINT_LOG_DIR` (logs, runner log directory),
`SPAWNPOINT_API_TOKENS` (auth disabled if unset).

## Testing

```bash
python -m unittest discover -s tests -v
```

Storage layer uses sqloader (SQLite backend, pinned to `sqloader==0.2.18` in requirements.txt).
Other modules use only Python standard library.
Install: `pip install -r requirements.txt`

## Out of Scope (Deferred by Design)

- State transitions after `created` (active/expired): instance lifecycle management module (L-0006 §3)
- Physical DB engine choice, retention/deletion policy for expired instances (DB-0007 [DEFERRED])
- This implementation's reference storage is SQLite; schema, constraints, indices follow DB-0007 exactly.

## Runner (Process Runner) UI

The screen served at `GET /` (`runnerview/static/index.html`) is wired to the actual `/processes*` API
provided by `spawnpoint/runner.py`'s `ProcessManager`. When you enter a command on screen and run it,
an actual OS subprocess starts. Stop/restart buttons terminate/restart the actual process tree. The log
panel polls `logs/<id>.log` every 1 second. Default local mode enables all features immediately — no
separate token input needed.

Arbitrary OS command run/stop/restart/list, log tail, POSIX killpg / Windows taskkill /T are
a separate domain from spawn instance registration (above). No higher-level design documents (D/P/L/DB chain)
exist yet — `runner.py` operates independently from that chain.

> Note: Previously this screen was a separate app (`app/runner_main.py`, port 8092),
> but there was no reason to run two processes, so it was integrated into the main server (`app/main.py`).
> Next, the features advertised by the screen (actual run/stop/restart/logging) were wired up—
> previously it was only mockup data.
