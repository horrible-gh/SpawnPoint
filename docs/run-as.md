# Command editor and execution accounts

The command dialog implements the approved layout: Name / Working directory,
full-width Command, execution-account selector beside Auto start, conditional
Username / Password, collapsible key/value environment rows, and Cancel / Save /
Save & Run (or Save & Restart). Preview controls and mock data are not shipped.

## Accounts

Service account preserves the existing execution path. Specific user runs the
command with the selected OS identity; this does not create a desktop login.
Windows accepts local usernames, `DOMAIN\username`, and UPN names. Linux accepts
an existing local/system account supported by the machine's `su` PAM policy.
Account authentication and account policy are checked at every execution, including
automatic start. Incorrect credentials never fall back to the service account.
There is a ten-second per-account failure cooldown and no automatic password retry.

Save stores configuration without executing the command. A password is required
for a new/changed account; leaving it blank while editing the same account retains
the saved credential. Choosing Service account removes the credential from the
active record. Existing SQLite pages/backups may still contain old encrypted data;
this is not a cryptographic-erasure guarantee. Changing the OS password requires
updating the saved password. Existing running processes retain their identity until
restarted. A failed durable identity update does not change the active configuration.

The API uses additive `run_as`: `{ "mode": "service" }` or
`{ "mode": "user", "username": "account", "password": "..." }` (Windows optional
`domain`). Omission on update preserves the existing identity. Responses contain
only `mode`, `username`, optional `domain`, and `has_password`—never the credential.
`GET /runtime` reports the server OS, service account, and current availability.

## Windows

Windows authenticates with CreateProcessWithLogonW and loads the target profile.
The target must have the applicable OS logon rights, access to SpawnPoint's executable,
and access to its working directory. Secondary Logon must be available. Windows
Hello PINs are not account passwords. The installer uses LocalService; LocalSystem
is not a supported caller for this API. No blanket elevation or UAC bypass is added.
A suspended supervisor is placed in parent-owned job objects before the user command
starts. Stop/Restart therefore operate on the whole supervised process tree.

Credentials use user-scoped Windows DPAPI. Changing SpawnPoint's own service identity
or moving its database to a different machine requires reentering passwords.

## Linux

Run SpawnPoint under its existing non-root service account. The feature requires
`/usr/bin/su` (util-linux) and the system PAM configuration. Root mode is deliberately
rejected for this feature because root `su` can bypass password authentication.
No sudoers entry, root web service, or custom setuid binary is installed.

Passwords travel through a private input pipe, not argv or environment variables.
The authenticated supervisor uses the target HOME, USER, LOGNAME, and system PATH;
it does not source shell startup scripts. Set PATH explicitly in Environment when
a tool is installed in a user-specific directory. The default working directory is
the target home, unless Working directory is specified. Both the executable and
working directory must be accessible to the target account.

Linux credentials use AES-256-GCM. The random key lives separately at
`<SPAWNPOINT_DB_PATH>.credentials/master.key`; directory/file permissions are 0700 /
0600, owned by the SpawnPoint service user. This is a protected key file, **not** a
desktop keyring. Back up the key separately from SQLite. Missing/wrong keys fail
closed; reenter the password. Compromise of the service account or root can expose
the key and credentials; encryption does not defend against a compromised host.

## Transport and access boundary

This is an arbitrary-command runner, not a multi-user authorization service.
Restrict access to trusted operators. Credential operations are allowed over direct
localhost access, or HTTPS with the existing Bearer-token authentication enabled.
The Host header must also be localhost for the local exception. Cross-origin writes
are rejected, and credential bodies require JSON. API tokens entered in the UI are
kept only in tab memory, not localStorage.

For a trusted TLS reverse proxy on the same machine, explicitly set
`SPAWNPOINT_TRUST_LOOPBACK_PROXY=1` in addition to `SPAWNPOINT_API_TOKENS`. Only a
loopback peer's `X-Forwarded-Proto: https` is trusted; configure the proxy to overwrite
that header. Do not expose an unauthenticated runner to the network.

## Upgrade

Build using the existing `tools/dist.ps1` / `tools/dist.sh`, replace the executable,
and restart SpawnPoint. Migration 005 adds the identity column without changing
existing commands or their automatic-start preferences. No existing data is deleted.
The retained HTML under `docs/ui` is a design reference only, not a production route.
