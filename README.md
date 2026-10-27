# Playground Control

[![CI](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/ci.yml/badge.svg)](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/ci.yml)
[![CodeQL](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/codeql.yml/badge.svg)](https://github.com/asyncsquadlabs/playgroundcontrol/actions/workflows/codeql.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-0c6478.svg)](LICENSE)

Playground Control is a self-installing Windows service and private-LAN dashboard for household game-permission control. A parent can allow or block play from a phone; while play is blocked, a mandatory Roblox launch guard intercepts known Roblox player and launcher images before they start, and the service closes configured game processes.

> **This is a household control, not a kernel-level security boundary.** It is designed for a standard, non-administrator player account on a trusted private LAN. See [SECURITY.md](SECURITY.md) before deployment.

## Status

- **Version:** 1.0.0
- **Target:** Windows 10/11 x64
- **Distribution:** single self-installing EXE
- **Runtime:** Go 1.26.10 or later required for source builds
- **Signing:** release binaries are unsigned unless a signing command is provided

## Features

- Single EXE that installs a LocalSystem Windows service.
- Embedded responsive LAN dashboard on TCP port 8080.
- Six to twelve digit administrator PIN hashed with Argon2id.
- First-run setup restricted to the local PC and a one-time 256-bit token.
- Persistent play permission and dashboard-managed game list.
- **Mandatory Roblox guard** — no Roblox entry is required in the custom list:
  - `RobloxPlayerBeta.exe`
  - `RobloxPlayer.exe`
  - `RobloxPlayerLauncher.exe`
  - `RobloxPlayerInstaller.exe`
  - `RobloxApp.exe`
  - `Roblox.exe` (Store alias)
- IFEO launch interception while play is blocked, with automatic guard removal when play is enabled.
- Detection of renamed Roblox processes running from known Roblox install paths.
- Graceful close request, followed by a 10-second grace period and force-close fallback for custom games.
- PID plus process-creation-time identity checks to avoid terminating reused PIDs.
- Windows Firewall rule limited to inbound TCP 8080 on the private profile and local subnet.
- Protected ProgramData storage, audit rotation, login throttling, CSRF protection, security headers, and fail-closed configuration handling.
- Recovery commands for setup, PIN reset, and uninstall.
- One-command test, build, resource, signing, and static-analysis workflows.

## Quick start (Windows)

1. Download `PlaygroundControl-1.0.0-win-x64.zip` from the repository's Releases page.
2. Verify the SHA-256 shown in the release notes.
3. Extract the ZIP to a local folder.
4. Run `PlaygroundControl.exe` and approve the UAC prompt. If Windows SmartScreen appears, verify the hash before choosing **More info → Run anyway**.
5. Complete the first-run setup on the gaming PC and choose an 8 to 12 digit PIN.
6. Open the dashboard from a phone on the same private LAN using the URL shown by setup.
7. Read [TESTING.html](TESTING.html) for the 23-item acceptance test.

If an earlier build is already installed, uninstall it before running this build. The current installer does not perform in-place upgrades.

## How it works

```text
Parent phone / LAN browser
          │  HTTP + PIN session
          ▼
Playground Control service (LocalSystem)
     TCP 8080, private-LAN firewall rule
          │
          ├── Persistent config + audit log in C:\ProgramData\PlaygroundControl
          ├── IFEO launch guard for Roblox while play is off
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
internal/store/              Atomic persistence and bounded audit log
internal/windowsapp/         Installer, SCM, firewall, ACL, and recovery modes
winres/                      Icon, manifest, and version resources
TESTING.html                 Interactive Windows acceptance runbook
SECURITY.md                  Threat model, accepted risks, reporting process
```

## Configuration and recovery

The service owns these files under `C:\ProgramData\PlaygroundControl`:

- `config.json` — play state and custom game list
- `config.json.bak` — last successful configuration backup
- `auth.json` — Argon2id PIN record
- `setup.token` — short-lived one-time setup token when unclaimed
- `audit.jsonl` / `audit.1.jsonl` — rotating non-secret activity log
- `service.log` — service diagnostic log

The directory is ACL-restricted to SYSTEM and Administrators. It is deleted on uninstall after confirmation, along with the service, firewall rule, PIN, game list, and audit history.

Local administrator recovery commands:

```text
PlaygroundControl.exe --setup         recreate the local setup link
PlaygroundControl.exe --reset-admin   reset the administrator PIN
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

The 23-item runbook in [TESTING.html](TESTING.html) covers installation, UAC, SCM registration, ACLs, first-run PIN setup, LAN access, the Roblox pre-launch guard, game closure, persistence, security boundaries, and uninstall.

Automated tests also run under Wine in development for:

- Real Windows process enumeration and identity-checked termination
- Graceful `WM_CLOSE` handling
- IFEO registry guard set/remove and pre-existing debugger restoration
- HTTP API, authentication, CSRF, origin, Host, and security-header checks

A real Windows 10/11 x64 acceptance run is still required before production deployment, particularly for UAC, SCM, firewall, ACL propagation, IFEO launch interception, and reboot recovery.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues privately through GitHub Security Advisories, not public issues.

## License

MIT. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
