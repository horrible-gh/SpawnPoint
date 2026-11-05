# Registered-command automatic start

In **New command** or **Edit command**, enable **Auto start** and save. Enabled commands have a small power icon beside their name in the process list. New and existing registrations default to off.

The flag is independent of the dialog buttons:

- **Save** only saves. Enabling Auto start does not start a stopped command immediately.
- **Save & Run** starts a new command now as before.
- **Save & Restart** restarts an existing command now as before.

On the next SpawnPoint startup, after configuration, database migration, registration restore and successful listener binding, opted-in commands are started once. A failed command does not prevent subsequent commands from starting. Non-opted-in commands remain stopped. Automatic start is not automatic recovery: exited, failed or manually stopped commands are not restarted in a loop.

Startup uses the ordinary runner start path and its running-entry guard. Shutdown cancels further automatic starts and waits for any in-flight start before taking the child cleanup snapshot. The preference is stored in SQLite by migration `004_runner_auto_start.sql`; existing registration data is preserved.

## API extension

`POST /processes` and `PUT /processes/{id}` accept optional `auto_start: true|false`. Null, strings, numbers, arrays and objects are rejected with HTTP 400 and `error.field = "auto_start"`.

On creation, omission defaults to false. On update, omission preserves the previous flag for compatibility with existing clients. Explicit false disables it. The existing POST `start` flag independently controls immediate execution.

Process responses carry `auto_start: true` when enabled. The field is omitted when false to preserve legacy response bodies; consumers must interpret absence as false. `persisted` must be true on a write response before relying on the preference surviving a restart.

```json
{"label":"worker","cmd":"worker-command","start":false,"auto_start":true}
```

This registers a stopped command that runs on future SpawnPoint starts. It does not configure operating-system startup of SpawnPoint itself.

## Boundaries

No readiness/dependency ordering or restart-on-crash policy is introduced. Attempts follow the existing registration ordering, but later commands do not wait for earlier applications to become ready.

The duplicate-start guard covers an entry currently managed by this SpawnPoint instance, not an independently started OS process. Keep the normal child-cleanup policy when using automatic start. With `SPAWNPOINT_KILL_CHILDREN_ON_EXIT` disabled, surviving children are not adopted after a parent restart, so enabling automatic start for those entries can create duplicates. Old PIDs are deliberately not trusted or killed.

## Regression checks

Run the normal Go suite and the optional real-child checks in an isolated test environment:

```sh
go test ./...
SPAWNPOINT_LIVE_SPAWN=1 go test ./internal/runner -run AutoStart -count=1
node tools/test-autostart-ui.cjs
```

Tests use temporary databases and throwaway shell commands, never the deployed server's registrations.
