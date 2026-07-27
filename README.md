# SpawnPoint

SpawnPoint is a lightweight service for creating uniquely identified task instances and managing local subprocesses from a browser. The API, Runner UI, and process manager run together in one Python process on one port.

![SpawnPoint dashboard](assets/images/SpawnPoint.png)

## Features

- Validates spawn requests and allocates collision-resistant instance IDs.
- Deduplicates repeated requests within a configurable time window.
- Persists spawn records and Runner commands in SQLite.
- Starts, stops, restarts, updates, and deletes OS subprocess registrations.
- Streams combined process output through a polling log API.
- Serves the Runner UI and JSON API from the same HTTP server.
- Supports optional Bearer-token authentication.

## Quick start

SpawnPoint requires Python and the dependencies listed in `requirements.txt`.

```bash
pip install -r requirements.txt
python -m app.main
```

The server starts on `http://127.0.0.1:8091` by default:

- Runner UI: `GET /`
- Health check: `GET /healthz`
- Spawn API: `POST /spawn`
- Process API: `/processes*`

Authentication is disabled when `SPAWNPOINT_API_TOKENS` is unset. For a token-protected deployment:

```bash
SPAWNPOINT_API_TOKENS="tok-a,tok-b" \
SPAWNPOINT_DB_PATH=/var/lib/spawnpoint.db \
python -m app.main
```

### Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `SPAWNPOINT_HOST` | `127.0.0.1` | HTTP bind address |
| `SPAWNPOINT_PORT` | `8091` | HTTP port |
| `SPAWNPOINT_DB_PATH` | `spawnpoint.db` | SQLite database path |
| `SPAWNPOINT_LOG_DIR` | `logs` | Runner process log directory |
| `SPAWNPOINT_API_TOKENS` | unset | Comma-separated allowed Bearer tokens; auth is disabled when unset |
| `SPAWNPOINT_KILL_CHILDREN_ON_EXIT` | `1` | Set to `0` to leave Runner processes alive when the server exits |

Starting another server on an occupied port fails immediately with exit code 1 and an explanatory message.

## Spawn API

`POST /spawn` accepts JSON. When authentication is enabled, include `Authorization: Bearer <token>`.

```json
{
  "requester": "user-a1b2c3",
  "kind": "session",
  "request_key": "req-7f3a90",
  "options": {
    "label": "nightly-build",
    "ttl_seconds": 3600
  }
}
```

A successful response contains the allocated instance:

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

Instance IDs use `spwn_{YYYYMMDD}_{seq}{rand}`: a daily atomic sequence, zero-padded to four digits, followed by a six-digit hexadecimal suffix. The sequence and database primary key provide the uniqueness guarantees; collisions are retried.

Repeated `request_key` values received within the 300-second deduplication window return the existing instance with `"deduplicated": true`.

### Error responses

| Condition | HTTP status | Error code |
| --- | ---: | --- |
| Invalid request | 400 | `invalid_request` |
| Missing or invalid token | 401 | `unauthorized` |
| Request body over 64 KiB | 413 | `payload_too_large` |
| Storage failure | 500 | `storage_error` |
| Unknown route | 404 | `not_found` |
| Unsupported method | 405 | `method_not_allowed` |

Errors use this shape:

```json
{
  "ok": false,
  "error": {
    "code": "invalid_request",
    "field": "requester",
    "message": "..."
  }
}
```

## Process Runner

The dashboard uses the `/processes*` routes to manage real OS subprocesses. These routes follow the same authentication policy as `/spawn`.

| Method | Route | Purpose |
| --- | --- | --- |
| `GET` | `/processes` | List registered processes and their current state |
| `POST` | `/processes` | Register and start a process |
| `PUT` | `/processes/<id>` | Update a registration and start it |
| `DELETE` | `/processes/<id>` | Delete a registration |
| `POST` | `/processes/<id>/run` | Run a stopped registration |
| `POST` | `/processes/<id>/stop` | Stop the process tree |
| `POST` | `/processes/<id>/restart` | Restart the process with the same ID |
| `GET` | `/processes/<id>/logs?offset=<bytes>` | Read log output from a byte offset |

Start a process with a JSON body such as:

```json
{
  "cmd": "python worker.py",
  "label": "worker",
  "cwd": "/srv/app",
  "env": {
    "MODE": "development"
  }
}
```

Only `cmd` is required. When `label` is omitted, SpawnPoint uses the first token of the command. Commands run with `shell=True`, and combined stdout and stderr are appended to `logs/<id>.log`. The UI polls logs once per second.

Process-tree termination is platform-aware: POSIX uses process-group signals, while Windows uses `taskkill /T /F`. On Windows, child processes are also assigned to a job object with `KILL_ON_JOB_CLOSE` so the default cleanup policy still applies after a hard server shutdown.

### Persistence and restart behavior

Runner registrations (`id`, `label`, `cmd`, `cwd`, and `env`) are stored in the same SQLite database as spawn data and reloaded on startup. Log files remain readable because they are keyed by registration ID.

Live process state is intentionally not restored. A saved PID may have been reused by the operating system, so reloaded entries start as `stopped` with `pid: null`. Use `POST /processes/<id>/run` or the dashboard Run button to start them again.

By default, SpawnPoint terminates Runner children when the server exits. Setting `SPAWNPOINT_KILL_CHILDREN_ON_EXIT=0` leaves them running, but a restarted server cannot control those old processes because it restores registrations rather than unsafe PID ownership assumptions.

## Architecture

The spawn request pipeline is:

```text
Intake → Validator → Allocator → Registrar → Responder
```

```text
app/
  main.py                     Application entry point and server wiring
runnerview/
  page.py                     Static screen loader
  static/index.html           Runner UI
spawnpoint/
  allocator.py                Instance ID allocation
  auth.py                     Bearer-token validation
  clock.py                    KST display and UTC storage utilities
  http_api.py                 HTTP adapter and route handlers
  models.py                   Spawn instance model
  params.py                   Request parameters
  results.py                  Response builders
  runner.py                   Subprocess lifecycle management
  service.py                  Top-level spawn handler
  storage.py                  SQLite-backed registry
  validator.py                Request validation
  sql/                        sqloader queries and migrations
tests/                        Unit and socket-level integration tests
```

`app/main.py` loads the static dashboard from `runnerview` and injects it into the HTTP adapter. The Runner and spawn registration domains share a server and database but remain separate in their service logic.

## Testing

Run the full test suite with:

```bash
python -m unittest discover -s tests -v
```

The storage layer uses the SQLite backend from `sqloader==0.2.18`; the remaining application modules use the Python standard library.

## Scope

SpawnPoint currently provides instance creation and Runner process management. Instance lifecycle transitions after `created`, retention policies, expired-record deletion, and alternative database engines remain outside the current scope.