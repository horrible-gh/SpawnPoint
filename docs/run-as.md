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

The existing installer continues to run SpawnPoint as **LocalService**. No service
account change, blanket elevation, UAC bypass, or additional privilege is installed.
Callers with an already-enabled SeImpersonatePrivilege use LogonUserW followed by
CreateProcessWithTokenW. Other callers use CreateProcessWithLogonW. The choice is
made before authentication; a rejected password is never retried through another API.
Both paths request the target user's profile. LocalService is the tested Windows
service configuration; LocalSystem deployments are not part of the supported setup.

The target must have the applicable OS logon rights, access to SpawnPoint's executable,
and access to its working directory. Secondary Logon must be available. Windows
Hello PINs are not account passwords. Account policies still apply.

For token-based service execution, the caller creates a private noninteractive
desktop before the child starts. Its ACL permits SYSTEM, the caller, and only the
newly authenticated logon-session SID. Window-station access is granted to that
same unique logon SID, without WRITE_DAC or WRITE_OWNER. The server's existing
desktop is not opened to the target. The temporary station ACE is revoked and the
private desktop handle is closed on launch failure or process exit. Cleanup failure
is reported without secrets; a surviving ACE names that particular logon session,
not all future sessions of the account.

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

A same-machine Caddy TLS proxy is recognized automatically with its default
forwarding headers: the socket peer must be loopback, the scheme must be a single
`https`, the forwarded Host must match the request Host, and the forwarded client
IP list must be valid. No environment opt-in is required for this configuration.
Authentication is still required; the access-control gateway also accepts its
managed user session. Login, cookies, origin checks and execution use one shared
transport policy. A forwarded HTTP request never receives the direct-local exception.

This trusts local OS processes and the local proxy. The proxy must overwrite client
forwarding headers, as Caddy does by default. Remote/LAN socket peers are never
trusted based on these headers. `SPAWNPOINT_TRUST_LOOPBACK_PROXY=0` explicitly opts
out; `1` retains the legacy opt-in for a local proxy supplying only the scheme.
Do not expose an unauthenticated runner to the network.

## Upgrade

Build using the existing `tools/dist.ps1` / `tools/dist.sh`, replace the executable,
and restart SpawnPoint. Migration 005 adds the identity column without changing
existing commands or their automatic-start preferences. No existing data is deleted.
The retained HTML under `docs/ui` is a design reference only, not a production route.

## Verification

Implementation commit `79ea50a1336b4fe9726ae48a210e32353a0fab6e` passed both jobs in
[Actions run 42547870887](https://github.com/horrible-gh/SpawnPoint/actions/runs/42547870887)
on 2026-11-09. The matrix used GitHub-hosted Ubuntu and Windows Server 2025 runners.
This verifies the implementation on those environments, not every Windows domain,
PAM configuration, or the operator's production machine.

The workflow runs full Go builds and unit regressions, both distribution scripts,
existing automatic-start/UI checks, and Linux race checks. Native account tests
exercise valid and invalid passwords without a service-account fallback. Real server
tests cover identity, target environment, output, stop, editing with retained
credentials, encrypted SQLite, restore, automatic start, clearing the identity, and
deletion. Service tests install actual systemd and LocalService fixtures with the
shipped installers and verify account identity, descendant termination, service
restart, credential reuse, and automatic start. Isolated test accounts and service
fixtures are confined to the disposable CI runners.

The browser test uses a running server, saves through the approved editor, checks
response/password redaction, reopens the existing registration, and captures 390,
1440, and 2560-pixel layouts without JavaScript errors or dialog overflow. Screenshots
are attached to the workflow run; they are not mock-API renderings.
