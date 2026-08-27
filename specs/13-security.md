# 13 — Security & Privileges

Status: Draft · Auth model decision: OQ-7

## Threat model (single-box, self-hosted)

| Threat | Mitigation |
|---|---|
| Random internet scans | Bind `127.0.0.1` by default; token auth on by default |
| Token theft over network | TLS guidance (`server.tls = auto` self-signed, or reverse proxy); token never logged |
| Malicious job script escaping its identity | `run_as` privilege separation, process groups, umask/rlimits |
| Secret leakage via UI/API/logs/exports | `secret_env` masking everywhere, hashed in audit, stripped from exports |
| Malicious/compromised registered user (system mode) | `run_as` locked to owner uid; scoped API surface; per-source reload isolation (`17`) |
| Log-forging via job output | ANSI is rendered but never interpreted as HTML; log lines are data |
| CSRF on the API | UI stores token in memory only (no cookies v1) → CSRF is structurally moot; if cookie sessions arrive (OQ-7), they get `SameSite=Strict` + CSRF tokens |
| Docker socket abuse (Mode B) | Explicit config only, never auto-discovered; docs carry root-equivalence warning |

## Authentication (OQ-7 — recommended: single bearer token)

- **Local channel**: `minicron.sock` (0600 in the 0700 data dir) +
  peer-credential check (uid match or root). CLI uses this transparently —
  zero-config, zero-secrets local operation. In system mode the socket
  moves to `/run/minicron/minicron.sock` (0660, `minicron` group) and
  the peer uid maps to a registered user — registration is the gate (`17`).
- **Network channel**: `Authorization: Bearer <token>`.
  - Token generated on first boot: 32 bytes base64url, stored `0600` at
    `data_dir/token` (gitignored path, never in config file).
    `minicron token` (socket/local only) prints or `--rotate`s it.
    Env override `minicron_TOKEN` for container deployments.
  - `minicron_AUTH=off` disables auth entirely — allowed **only** when
    binding a loopback/unix address; the daemon refuses `auth=off` on a
    non-loopback bind (foot-gun guard).
- Login throttling: 10 failed attempts / minute / source IP, then 5 min
  lockout; attempts logged.
- Later candidates (post-v1): cookie sessions with passwords, read-only
  roles. Multi-user *is* in scope via system mode (`17`); the single-admin
  token applies to user mode.

## Privilege model (D-5)

- Daemon may run as root (typical: installed service) or unprivileged.
- `run_as` per job/worker (spec `07`): resolved at spawn, applied
  `setgroups → setgid → setuid` **after** `chdir`, before `exec`.
  Non-root daemon + foreign `run_as` = config validation error, upfront.
- Data-dir hygiene: `0700` dir, `0600` db/socket/token/logs; enforced (and
  reported) at boot even on pre-existing dirs.
- The daemon itself never needs capabilities beyond its user (no
  CAP_SYS_ADMIN tricks; cgroup features, if ever added, degrade gracefully).

## Secrets handling

- `secret_env`: values sourced from `${file:path}` or env refs; stored in
  SQLite **only as references** (the resolved map exists in process memory
  at spawn time, never persisted, never logged, never returned by API —
  masked `•••` with key names intact).
- Audit/revision records store the definition with secret *references*
  unchanged.
- Exports strip to references with a provisioning checklist header
  (spec `05`).
- Env-var masking in log output is **not** attempted (impossible to do
  correctly); instead: docs recommend secrets never be echoed, and
  `command` review is a PR-time concern.

## HTTP hardening (all responses)

- `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, strict CSP
  (`default-src 'self'`; no inline scripts — hashed or bundled), referrer
  policy.
- Request caps: body 1 MiB (import bundles validated to 10 MiB multipart),
  header 32 KiB, URL 2 KiB; SSE connection caps per `11`.
- No user-controlled server-side file paths outside validated config
  (exports write to client downloads; nothing on disk reads request paths).

## Supply chain & release integrity

- Release artifacts signed (cosign) + `sha256sums.txt`; install script
  verifies checksums and pins the requested version.
- Dependencies: minimal, audited-on-release (`govulncheck` for Go, or
  `cargo audit`/`cargo deny` if OQ-1 lands Rust) in CI.
- Disclosure: `SECURITY.md`, private reporting via GitHub advisories.

## What we deliberately do NOT do v1

- No CHAP/derived-key handshake (the reference product's approach): bearer
  token + TLS guidance is proportionate for a single-admin, single-box tool
  and far simpler to reason about (OQ-7 records the trade-off).
- No sandboxing/seccomp for job processes (beyond rlimits + uid): the tool
  runs *your* scripts; isolation is a container story (docker driver).

## Open questions

- OQ-7: confirm single-token model (recommended) vs token + optional
  password-cookie sessions.
- Should `token` file support multiple tokens (e.g. a read-only one)?
  Recommendation: v2, schema reserves `token_id` claim space.
