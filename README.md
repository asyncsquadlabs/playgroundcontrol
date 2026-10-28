# Playground Control

[![CI](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/ci.yml/badge.svg)](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/ci.yml)
[![CodeQL](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/codeql.yml/badge.svg)](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/codeql.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-0c6478.svg)](LICENSE)

Playground Control is a self-installing Windows service and private-LAN dashboard for household game-permission control. A parent can allow or block play from a phone; while play is blocked, a mandatory Roblox launch guard intercepts known Roblox player and launcher images before they start, and the service closes configured game processes.

> **This is a household control, not a kernel-level security boundary.** It is designed for a standard, non-administrator player account on a trusted private LAN. See [SECURITY.md](SECURITY.md) before deployment.

## Status

- **Version:** 1.2.2
- **Target:** Windows 10/11 x64
- **Distribution:** single self-installing EXE
- **Runtime:** Go 1.26.10 or later required for source builds
- **Signing:** release binaries are unsigned unless a signing command is provided

## Features

- Single EXE that installs a LocalSystem Windows service.
- Embedded responsive LAN dashboard on TCP port 8080.
- Six to twelve digit administrator PIN hashed with Argon2id.
- First-run setup restricted to the local PC and a one-time 256-bit token.
- PIN setup, play permission, game list, and audit history persist across service restarts and Windows reboots in SQLite.
- **Mandatory Roblox guard** — no Roblox entry is required in the custom list:
  - `RobloxPlayerBeta.exe`
  - `RobloxPlayer.exe`
  - `RobloxPlayerLauncher.exe`
  - `RobloxPlayerInstaller.exe`
  - `RobloxApp.exe`
  - `Roblox.exe` (Store alias)
- IFEO launch interception while play is blocked: a blocked Roblox launch opens a Google Chrome search for a fixed study guide, with a default-browser fallback. The guard is removed automatically when play is enabled.
- Detection of renamed Roblox processes running from known Roblox install paths.
- Graceful close request, followed by a 10-second grace period and force-close fallback for custom games.
- PID plus process-creation-time identity checks to avoid terminating reused PIDs.
- Windows Firewall rule limited to inbound TCP 8080 on the private profile and local subnet.
- Protected SQLite ProgramData storage, last-known-good database backup, login throttling, CSRF protection, security headers, and fail-closed configuration handling.
- Live native setup window with a step-by-step transcript, service-start checkpoints, and copyable diagnostics.
- In-place upgrade/repair that preserves the PIN, games, play state, and history, plus an explicitly confirmed full reset.
- Recovery commands for setup, PIN reset, repair, reinstall, and uninstall.
- One-command test, build, package, signing, and static-analysis workflows.

## Quick start (Windows)

1. Download `PlaygroundControl-1.2.2-win-x64.zip` from the repository's Releases page.
2. Verify the SHA-256 shown in the release notes.
3. Extract the ZIP to a local folder.
4. Run `PlaygroundControl.exe` and approve the UAC prompt. If Windows SmartScreen appears, verify the hash before choosing **More info → Run anyway**. The setup window shows each installation step; when an older version is detected, choose **Repair / keep data** to upgrade in place.
5. Complete the first-run setup on the gaming PC and choose an 8 to 12 digit PIN.
6. Open the dashboard from a phone on the same private LAN using the URL shown by setup.
7. Read [TESTING.html](TESTING.html) for the 24-item acceptance test.

If an earlier build is already installed, run the 1.2.2 installer and choose **Repair / keep data** to upgrade in place. The installer uses a live setup window, preserves `C:\ProgramData\PlaygroundControl`, and reports the exact Windows service error if startup fails. Choose **Full reset** only when you intentionally want to delete the PIN, games, play state, and history. A preserved 1.0.x ProgramData directory is still imported into SQLite once and the old files are archived as `*.migrated`.

## How it works

```text
Parent phone / LAN browser
          │  HTTP + PIN session
          ▼
Playground Control service (LocalSystem)
     TCP 8080, private-LAN firewall rule
          │
          ├── SQLite state + bounded audit history in C:\ProgramData\PlaygroundControl\playground.db
          ├── IFEO launch guard for Roblox while play is off, redirecting blocked launches to a Chrome study search
          └── Process enforcement every 500 ms
                 ├── close request in the player's session
                 └── force-close after 10 seconds
```

The service runs in session 0. For graceful closes, it acquires the interactive user's token, launches a narrow close helper in that user's session, and posts `WM_CLOSE` to visible top-level windows. Force-kill remains available to the service after the grace period.

### Defaults

| Setting | Default |
|---|---|
| Play permission | Blocked |
| HTTP port | 8080 |
| Process scan interval | 500 ms |
| Grace before force-close | 10 seconds |
| Dashboard host access | Private/loopback source addresses only |
| Firewall scope | Inbound, TCP 8080, private profile, local subnet |
| Setup token lifetime | 15 minutes, one use |
| Admin session | 30-minute idle, 12-hour absolute |
| Login throttle | 5 failures / 15 minutes per source IP |
| Persistence | SQLite WAL, full synchronous commits, latest-3 corrupt archives |
| Audit retention | 50,000 newest events |

## Build from source

### Requirements

- Go 1.26.10 or newer (also selected automatically from `go.mod`).
- Node.js 22+ for the JavaScript syntax check (`make check`).
- `zip` if you want to create a distributable archive manually.
- Optional: [`go-winres`](https://github.com/tc-hib/go-winres) for regenerating Windows icon/version resources, and an Authenticode certificate for signing.

### Commands

```sh
# Tests, vet, JavaScript syntax, and Windows cross-vet
make check

# Race-enabled tests
go test -race ./...

# Static and dependency security checks (requires govulncheck, gosec, staticcheck)
make security

# Build dist/PlaygroundControl.exe and its SHA-256
make build

# Build the versioned Windows ZIP and both checksum files
make package

# Run checks, build, and package
make release

# Regenerate the embedded Windows resource object after changing icon/version data
make resources

# Optional Authenticode signing
# Set CERT_PASSWORD in your environment; never commit signing material.
make sign SIGN_CMD='signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 /f cert.pfx /p "$CERT_PASSWORD"'
```

The build is deterministic apart from Go's normal build metadata. The generated executable is written to `dist/`, which is intentionally ignored by Git and should be published as a release asset rather than committed.

## Project layout

```text
cmd/playgroundcontrol/       Windows entry point and generated resources
internal/auth/               PIN hashing, setup tokens, sessions, throttling
internal/enforce/            Process-detection and close/kill state machine
internal/model/              Configuration, validation, and audit types
internal/platform/           Windows process APIs, IFEO guard, close helper
internal/protection/         Mandatory Roblox image and path policy
internal/server/             HTTP API, security middleware, embedded dashboard
internal/store/              SQLite persistence, recovery, migration, bounded audit log
internal/windowsapp/         Installer, SCM, firewall, ACL, and recovery modes
winres/                      Icon, manifest, and version resources
TESTING.html                 Interactive Windows acceptance runbook
SECURITY.md                  Threat model, accepted risks, reporting process
```

## Configuration and recovery

The service owns these files under `C:\ProgramData\PlaygroundControl`:

- `playground.db` — SQLite state: Argon2id PIN record, play permission, custom game list, and bounded audit history
- `playground.db.bak` — last-known-good SQLite snapshot, refreshed after PIN/config/game changes and on clean shutdown
- `playground.db-wal` / `playground.db-shm` — SQLite write-ahead log and shared-memory files while the service runs
- `setup.token` — short-lived one-time setup token when unclaimed
- `service.log` — service diagnostic log, including startup checkpoints
- `install.log` — installer transcript (rotated at 2 MiB)
- `*.migrated` — archived JSON/JSONL files after the one-time upgrade import

The database uses WAL mode and full synchronous commits. A corrupt or unreadable primary database is archived with a timestamp suffix, the last-known-good backup is restored when valid, and a fresh setup is offered only when no usable backup exists. The latest three corrupt archives are retained. The directory is ACL-restricted to SYSTEM and Administrators. It is deleted on uninstall after confirmation, along with the service, firewall rule, PIN, game list, and audit history.

Browser sessions remain in memory and do not survive a service or Windows restart. After a reboot, open the dashboard and sign in with the existing PIN; do not create a new one. An unreadable authentication record is a startup error rather than an automatic reset, so `--reset-admin` remains the explicit recovery path.

If setup reports that the service did not start, the progress window shows the last startup phase, the Windows service exit code, the tail of `service.log`, and recent Event Log entries. Choose **Repair / keep data** first; if the database or ACL state is irrecoverable, choose **Full reset** after reviewing the destructive confirmation. The installer preserves `install.log` and copies it to `%TEMP%` before a full reset deletes ProgramData.

Local administrator recovery commands:

```text
PlaygroundControl.exe --setup         recreate the local setup link
PlaygroundControl.exe --reset-admin   reset the administrator PIN (keeps play state, games, and audit history)
PlaygroundControl.exe --repair        rebuild the service and application files, preserving all data
PlaygroundControl.exe --reinstall     full reset, then a fresh installation (asks for confirmation)
PlaygroundControl.exe --uninstall     remove the application and its data
PlaygroundControl.exe --version
```

## Security

Security design, accepted risks, and the reporting process are documented in [SECURITY.md](SECURITY.md). In particular:

- The dashboard is plain HTTP and must only be used on a trusted private LAN.
- The player must not have Windows administrator access.
- The EXE is unsigned unless you sign it; IFEO changes may trigger antivirus heuristics.
- Roblox enforcement is deterrence and household policy, not a kernel-level guarantee.
- Roblox Studio is not blocked by default.
- No software can be guaranteed free of vulnerabilities. This repository uses patched Go tooling, dependency vulnerability scanning, static analysis, CodeQL, and a substantial portable/Windows test suite, but findings should be reported privately.

### Security checks used

- `go test ./...`
- `go test -race ./...`
- `go vet ./...` for Linux and Windows targets
- `govulncheck ./...` with Go 1.26.10
- `gosec ./...`
- `staticcheck ./...`
- GitHub CodeQL for Go
- GitHub secret scanning and Dependabot alerts for public releases

## Testing on Windows

The 24-item runbook in [TESTING.html](TESTING.html) covers installation, UAC, SCM registration, ACLs, repair/full reset recovery, first-run PIN setup, LAN access, the Roblox pre-launch guard, game closure, persistence, security boundaries, and uninstall.

Automated tests also run under Wine in development for:

- Real Windows process enumeration and identity-checked termination
- Graceful `WM_CLOSE` handling
- IFEO registry guard set/remove and pre-existing debugger restoration
- SQLite persistence, backup recovery, and one-time JSON migration
- HTTP API, authentication, CSRF, origin, Host, and security-header checks

GitHub Actions also runs a Windows service-startup smoke test that creates the service, waits for `/api/v1/health`, captures diagnostics on failure, and removes the service. A real Windows 10/11 x64 acceptance run is still required before production deployment, particularly for UAC, the setup window, SCM, firewall, ACL propagation, IFEO launch interception, and reboot recovery.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately through GitHub Security Advisories, not public issues.

## License

MIT. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
