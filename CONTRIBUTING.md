# Contributing

Thanks for considering a contribution to Playground Control.

## Before opening a pull request

1. Open an issue for substantial behavior or security-boundary changes.
2. For vulnerabilities, follow [SECURITY.md](SECURITY.md) and use private reporting. Do not open a public security issue.
3. Keep changes focused and include tests for behavior changes.
4. Do not commit generated executables, archives, signing certificates, local configuration, logs, or setup tokens.

## Development

Requirements:

- Go 1.26.10 or newer. `go.mod` selects the patched toolchain when needed.
- Node.js 22+ for JavaScript syntax checks.
- Windows 10/11 x64 or Wine for Windows-specific tests.

Run the baseline checks:

```sh
make check
go test -race ./...
```

Optional security checks require the external tools:

```sh
make security
```

Build the Windows executable:

```sh
make build
```

## Windows integration changes

Installer, service-manager, firewall, ACL, SQLite schema/migration/recovery, IFEO, and close-helper changes need particular care. Include:

- The expected Windows version and edition.
- Exact administrator/standard-user context.
- Whether UAC, SCM, firewall, ACLs, and reboot recovery were tested on real Windows.
- Any audit or Event Viewer output without secrets.

Do not use administrator privileges to bypass safeguards that are part of the documented threat model. Do not add remote command execution, arbitrary process termination, unrestricted LAN exposure, or telemetry without an explicit design review.

## Code style

- Format Go with `gofmt`.
- Keep the dashboard dependency-free and compatible with the existing CSP.
- Preserve constant-time comparison for authentication secrets.
- Do not log PINs, setup tokens, session tokens, or signing credentials.
- Return generic errors to clients while retaining detail in local logs.
- Do not weaken the Host/private-network/CSRF checks.

By contributing, you agree that your contribution is licensed under the project's MIT license.
