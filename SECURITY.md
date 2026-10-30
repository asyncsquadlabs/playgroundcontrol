# Security Policy

## Supported versions

Security fixes are provided for the current 1.x release line.

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting:

https://github.com/asyncsquadlabs/playgroundcontrol/security/advisories/new

Do not open a public issue for a suspected vulnerability. Include:

- The affected version and SHA-256 of the executable.
- Windows version and edition.
- A concise reproduction or proof of concept.
- Expected and observed impact.
- Any relevant logs with PINs, setup tokens, and session tokens removed.

Reports are reviewed on a best-effort basis. This is an independent project, not
a commercial security response service.

## Threat model

Playground Control is a household parental-control tool for a **standard,
non-administrator Windows player account on a trusted private LAN**. It is not:

- A kernel-level DRM, firewall, or anti-cheat system.
- Protection against a Windows administrator.
- An Internet-facing service.
- A guarantee that every game or future launcher revision can be blocked.

The administrator who installs the service has full control of the PC and can
stop, remove, or otherwise override the product. A player who can obtain
administrator access is outside the protection boundary.

## Security design

- Initial setup is loopback-only and requires a random, one-time setup token.
- The administrator PIN is hashed with Argon2id using 64 MiB of memory,
  three iterations, and two lanes.
- Session and CSRF tokens are generated with `crypto/rand` and kept in memory.
- Login throttling is applied per source IP; sessions have idle and absolute
  expiry and a bounded session count.
- Mutating API calls require an authenticated session, a CSRF token, and an
  allowed origin. The service sends a restrictive Content Security Policy and
  related browser-hardening headers.
- The service listens only for private or loopback source addresses and checks
  the Host header against local interface addresses and the machine hostname.
- The built-in Roblox guard writes machine-wide Image File Execution Options
  entries while play is blocked. Toggling play on removes them, and uninstall
  restores any pre-existing debugger value.
- Enforcement runs as LocalSystem. The LAN dashboard uses a narrow authenticated
  HTTP API and has no privileged command endpoint.
- The installed service has a protected DACL granting full control only to
  SYSTEM and Administrators, so a standard player account cannot stop or
  reconfigure it through the Service Control Manager.
- ProgramData is owned by Administrators and protected by an explicit DACL
  granting access only to SYSTEM and Administrators. Reparse points are
  rejected. User-owned pre-existing directories are reset before use. The
  SQLite database, WAL/SHM files, and last-known-good backup inherit that DACL.
- The optional DeepSeek API key is sealed with machine-scoped Windows DPAPI
  before it is written to SQLite. The dashboard never returns the key, and the
  service never writes it to audit events or logs. AI monitoring is disabled by
  default.
- State changes are committed in SQLite transactions with WAL and full
  synchronous durability. A last-known-good snapshot is kept for recovery, and
  unusable databases are archived rather than overwritten. Initial setup is
  offered only when a healthy database has no administrator PIN row, unless no
  backup can be restored.
- The Windows firewall rule is inbound-only, TCP 8080, private profile, local
  subnet only.

## Known limitations and accepted risks

### Plain HTTP on the LAN

The dashboard is intentionally served over HTTP on port 8080. A party who can
observe or manipulate LAN traffic can capture a session cookie or the PIN during
login. The session cookie therefore cannot use the `Secure` attribute. Use this
project only on a trusted private network. Do not expose port 8080 to the
Internet or an untrusted Wi-Fi network. Adding TLS with certificate
provisioning is a possible future enhancement, not a current guarantee.

### Short numeric PIN

The PIN is 6 to 12 digits. Argon2id and per-IP throttling make online guessing
expensive, but the PIN does not have the entropy of a long password. Choose an
8 to 12 digit PIN for deployment.

### Local administrator

An administrator can stop the service, modify or remove the IFEO entries,
uninstall the product, or change the executable. No user-mode parental-control
service can prevent that.

### Game enforcement is not a security boundary

Roblox's known player and launcher images are intercepted by IFEO before launch,
and known Roblox install paths are checked as a fallback. Renaming or copying an
executable outside recognized install directories, using a future unlisted
launcher, using a Microsoft Store package whose launch path is not intercepted,
or using protected anti-cheat communication can evade enforcement. Roblox
Studio is not blocked by default.

### IFEO and antivirus

IFEO is a legitimate Windows debugging/restriction mechanism, but changing the
`Debugger` value is also a technique used by malware. Windows Defender or
third-party security software may warn about or block these registry writes.
The installed EXE is also unsigned in this release. Validate the release hash
and sign binaries for production deployment.

### DeepSeek cloud processing

AI monitoring is optional and off by default. When enabled, the service sends
the executable name, version-resource product/company/description, and an image
path with standard Windows user-profile segments redacted to DeepSeek over
HTTPS. It does not send window titles, command lines, usernames, SIDs, or
document names. DeepSeek's own retention and privacy terms apply to that
metadata. When the auto-block toggle is enabled (the default when AI monitoring
is enabled), a verdict at 0.90 confidence or higher is added to the blocked game
list and closed through the normal graceful-close path. Lower-confidence
findings are advisory and never trigger termination. False positives are
possible; removing an AI-added game or marking a finding **Not a game** writes a
local ignore entry so that executable/path is skipped entirely and is not sent
to DeepSeek again. Ignore entries are permanent and bounded to 5,000 rows.

### API key entry over plain HTTP

Any signed-in administrator can set or change the DeepSeek API key from the
dashboard. The dashboard is plain HTTP on the trusted LAN, so a party who can
observe LAN traffic while the key is being entered may capture it. Use the
dashboard only on a trusted private network, prefer entering the key from the
local PC, and rotate the key if compromise is suspected. The key is sealed with
machine-scoped DPAPI at rest but is necessarily decrypted in service memory to
call DeepSeek.

### Key portability

Machine-scoped DPAPI protects the API key against copying the database to
another computer. The tradeoff is that restoring the database onto a different
PC makes the saved key unreadable. The service starts normally, disables AI
calls, and asks the administrator to re-enter the key.

### No hardware-backed secrets

The SQLite database, PIN hash, configuration, AI settings, AI ignore entries,
AI key ciphertext, audit history, and backup are protected by Windows file ACLs
and, for the AI key, DPAPI. They
are not protected by a TPM or hardware security module. A local administrator
or offline attacker with disk access can replace or delete the data directory,
and machine-scoped DPAPI does not protect against a sufficiently privileged
local attacker. Sessions are intentionally memory-only and are not stored in
the database.

## Hardening recommendations

- Use a dedicated Windows 10/11 x64 computer or VM on a trusted private LAN.
- Use a standard account for the player and a separate administrator account.
- Choose an 8 to 12 digit PIN.
- Verify the SHA-256 of downloaded artifacts.
- Sign the executable before distributing it widely.
- Keep the Go toolchain, Windows, and dependencies patched.
- Monitor GitHub security advisories for this project.
