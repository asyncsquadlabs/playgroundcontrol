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
  rejected. User-owned pre-existing directories are reset before use.
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

### No hardware-backed secrets

The PIN hash and configuration are protected by Windows file ACLs, not a TPM or
hardware security module. A local administrator or offline attacker with disk
access can replace or delete the data directory.

## Hardening recommendations

- Use a dedicated Windows 10/11 x64 computer or VM on a trusted private LAN.
- Use a standard account for the player and a separate administrator account.
- Choose an 8 to 12 digit PIN.
- Verify the SHA-256 of downloaded artifacts.
- Sign the executable before distributing it widely.
- Keep the Go toolchain, Windows, and dependencies patched.
- Monitor GitHub security advisories for this project.
