# 17 — System Mode: Root Daemon, Registered Users

Status: Draft · Decision: ADR-4 · Extends D-5

## Overview

Two runtime modes (`[server] mode`, restart-only):

| | `user` (default) | `system` |
|---|---|---|
| Daemon runs as | any OS user | root (typically via `service install`) |
| Tenants | one (the daemon's user) | root/admin + any registered Unix user |
| Typical box | laptop, app server, container | shared server, homelab host |

Everything else in the specs assumes `user` mode unless stated. System mode
adds: user registration, per-user namespaces, per-user file sources, and an
authorization matrix — it changes nothing about scheduling, execution, or
storage mechanics.

## Registration & identity

- **`minicron user register`** — connects to the system socket; kernel
  peer credentials (`SO_PEERCRED` / `LOCAL_PEERCRED`) prove uid/gid — no
  passwords, no secrets. Records `{uid, username, gid, home}`. Idempotent:
  re-registering refreshes gid/home (user added to a group, renamed home).
- Companions: `minicron user whoami` (who the daemon thinks you are),
  `minicron user list` (admin), `minicron user unregister <name>` (admin;
  finalizes active runs, disables owned definitions, keeps history).
- Network access: **`minicron user token`** prints a user-scoped bearer
  token (rotatable). Users are CLI-first by design; web UI namespace
  scoping is v1.1 — v1 UI is admin-token, all scopes, owner badges shown.

Identity is the OS identity. A user is whoever the kernel says connected.

## Namespaces

- Definition identity is the pair (owner, name). The admin scope is the
  bootstrap config + its includes (owner = root).
- Display and CLI form: dotted — `alice.backup-db`, root's definitions
  shown bare (`backup-db`). The API accepts the dotted form, or `name` +
  `?owner=alice`. Job-name grammar per `01` applies to the bare name.
- Admin sees and manages every scope; a user sees and manages their own.

## User-owned sources & imports

- **`minicron import <path>`** run by a registered user registers that
  file as a **file-authority source in their scope** (spec `05` rules: the
  file is the single source of truth; reloads re-read it; UI/API cannot
  edit its metadata). `--copy` instead materializes **db-authority**
  definitions in their scope (editable later via UI/CLI as `05` allows).
- **`run_as` is locked to the owner** in user scopes: any other value is a
  validation error naming the line — never a silent override. `~` and
  `working_dir` resolve against the owner's home.
- Auto-scanning `~/.config/minicron/jobs/*.toml` per registered user is a
  MAY for later; explicit import first (no implicit magic).

## Authorization matrix

| Operation | Admin (root) | Registered user |
|---|---|---|
| List jobs — status, next fire, last run status | all scopes | own scope |
| View run logs (incl. live stream) | all | own runs |
| Trigger / stop / restart runs | all | own |
| Import file sources, create/edit db-authority definitions | any scope | own scope, `run_as` = self |
| Reload | all sources | re-validates & re-applies **own sources only** |
| Daemon settings, users, tokens, retention, storage | ✅ | ❌ |

Reload is scoped by ownership so a user's reload can never force
re-evaluation (and worker restarts) of root's or another user's definitions.

## Reload isolation

Every source — the bootstrap, each include, each user source — validates
**independently**. An invalid source keeps its previous definitions and
raises a scoped error (event + UI banner + CLI message to that owner);
every other source applies normally. One user's broken TOML never blocks
the box.

## Execution

- User-owned definitions spawn **as the owner**: `setgroups → setgid →
  setuid` to the registered uid/gid, `chdir` to the resolved working dir,
  umask/env per `07`. Supervised workers likewise run as the owner.
- Admin/bootstrap definitions keep the full `run_as` machinery of `07`
  (any user, root daemon).
- Log capture is unchanged — output flows through the daemon's sinks; users
  read their logs via CLI/API, never via filesystem access to the data dir.

## Socket & paths (system mode)

- Socket: `/run/minicron/minicron.sock`; parent dir `root:minicron`
  0755, socket 0660. The `minicron` group is created by
  `minicron service install`; membership = who may connect.
- No-group fallback: socket 0666 with the app-level peer-credential check —
  `SO_PEERCRED` is kernel-provided and unspoofable; non-registered uids are
  refused at the door. Either way, registration is the gate, not file
  permissions alone.
- Data dir stays `/var/lib/minicron` (root-owned, 0700) — including user
  logs, since users access them through the API/CLI.

## Storage impact (spec `08`)

- New `users` table: `(id, uid, username, gid, home, token_hash,
  registered_at)`.
- `definitions` gains `owner_user_id` (NULL = admin scope) and the
  `authority` column (`file` | `db`); uniqueness becomes
  `(owner_user_id, name)`.
- Audit `actor` values gain `user:<name>` alongside `file:<path>` / `ui` /
  `api` / `cli:<cmd>`.

## Security posture (details in `13`)

A compromised registered user can: run arbitrary code **as themselves**
(by design), read their own logs. Cannot: set `run_as` to anyone else,
touch other scopes, edit file-authority metadata, change daemon settings,
or read auth material. The daemon's own surface (root) is not reachable
through the socket beyond the operations above.

## Roadmap

System mode lands in **v0.2** (with `user register/token`, scoped reload,
per-source isolation, socket layout). It can be pulled into v0.1 if the
core lands early. The `users` table and `owner_user_id` column exist from
the v0.1 schema (always NULL) so no migration is needed.

## Open questions

- Should users be allowed to see *that* other scopes exist (names only,
  no detail) for discoverability? Draft: admin-only visibility v1.
- Per-user caps (max definitions / runs / log volume)? Draft: global caps
  only v1; per-user quotas MAY later.
