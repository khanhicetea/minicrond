# Codex review and decisions for the minicrond specifications

Date: 2026-08-26

Status: opinionated review of the draft suite in `./specs`. This file does not
supersede the product-owner constraints or accepted ADRs. It records the
implementation decisions I would make and the changes I believe are required
before coding starts.

## Executive verdict

The draft has a strong product direction: one self-contained daemon, local
process scheduling and supervision, SQLite metadata, useful run history, and a
small web UI. The authority model in ADR-3 is also a good foundation.

The suite is **not implementation-ready yet**. It currently has three major
problems:

1. **v0.1 is too large.** It combines a scheduler, supervisor, privilege
   changes, two configuration authorities, import/export, an HTTP API, SSE,
   an SPA, a binary log format, retries, four overlap policies, audit history,
   and service operations. Several v0.2/v0.3 features also leak into normative
   v0.1 examples.
2. **Some guarantees cannot be met by the stated design.** Examples include
   exact stdout/stderr interleaving, all descendants always dying with the
   daemon, resumable SSE IDs that are only per connection, browser EventSource
   bearer authentication, and a browser upload becoming a persistent
   file-authoritative source.
3. **There are cross-document contradictions.** Important ones include Go
   being selected while the architecture still says `tokio task`, zstd files
   being described as `.gz` readable with `zcat`, whole-reload atomicity versus
   per-file partial application, UTC versus local default timezone, and
   `timeout` being both a status and a subtype of another status.

My recommendation is to settle the decisions below, make one consistency pass
through all specs, write a small set of executable acceptance tests for the
state machines, and only then begin implementation.

## Decisions on every consolidated open question

| Question | Decision | Reason |
|---|---|---|
| OQ-1 language | **Go** | Best delivery speed and operational ecosystem after SlateDB was removed. Build with `CGO_ENABLED=0`; explicitly document the local `/etc/passwd`/`/etc/group` limits of pure-Go user lookup. |
| OQ-2 format | **TOML** | Fits multiline commands and strict configuration better than YAML. JSON remains the API and internal normalized representation. |
| OQ-3 authority | **Keep ADR-3** | File-linked and DB-native definitions must not silently overwrite each other. Refine browser import semantics as described below. |
| OQ-4 tables | **`[[job]]` and `[[worker]]`** | Natural for include files and generated exports. Duplicate names remain a validation error. |
| OQ-5 cron seconds | **No six-field cron** | Keep five fields. Permit sub-minute schedules only with `@every`, with a minimum interval of one second and normal concurrency limits. |
| OQ-6 catch-up default | **`none`** | Running delayed jobs can repeat destructive work. Record a summarized missed occurrence; do not execute by default. |
| OQ-7 auth | **One high-entropy bearer token plus Unix peer credentials** | Smallest understandable model. Store only a token hash, show a new token once, and use `sessionStorage` rather than persistent `localStorage` in the UI. Use authenticated `fetch()` streaming, not native `EventSource`, because EventSource cannot set an Authorization header. |
| OQ-8 port | **7423** | No material reason to spend more design time on it. Bind loopback by default. |
| OQ-9 UI | **Svelte 5 + Vite, static SPA** | Small output and good development ergonomics. This is a client-rendered SPA; remove claims that it works as server-rendered navigation without JavaScript. |
| OQ-11 name | **Keep `minicrond`** | Freeze the name now, subject to a package/domain/trademark check before the first public release. |
| OQ-12 license | **Apache-2.0** | Clear patent grant and one simple license. |
| OQ-13 file watch | **Manual reload in v0.1; opt-in watch later** | Correct rename/debounce/error behavior is not core. When added, default it off. |
| OQ-14 Docker | **One Docker driver for jobs and workers after the core release; no compose-service manager yet** | `docker run` for an ephemeral job or foreground worker is bounded. Managing pre-existing Compose projects creates a second reconciliation system. Defer Mode B and compose import until demonstrated demand, not merely to v0.3 by promise. |
| OQ-15 multi-host | **Explicit non-goal through v1** | Do not add clustering hooks beyond a stable daemon `instance_id` in S3 prefixes and exported metadata. |
| OQ-16 overlap | **`skip`** | Safest default. Ship only `skip` and `parallel` in the first core release; add `queue` and `replace` after their persistence semantics are tested. |
| OQ-17 notifications | **Inbox plus fixed-schema JSON webhook** | Do not ship SMTP, Slack-specific code, arbitrary templates, or log tails in webhook payloads initially. |
| OQ-18 missing source | **Default to disable, with an important distinction** | If an entire linked source disappears, retain and disable its definitions. If a valid, present source intentionally removes one definition, soft-delete that definition. These are not the same event. |
| OQ-19 metrics bind | **Main HTTP listener, authenticated** | It is simpler and Prometheus supports bearer tokens. Add a separate listener only on evidence of need. Health/readiness remain unauthenticated and disclose no details. |
| OQ-20 export bundle | **Single TOML by default** | Diffable, pipeable, and easy to inspect. ZIP can be added later; it is not needed for the first release. |

OQ-10 is already correctly resolved by ADR-1.

## Decisions on the parking-lot and document-local questions

- `retry_on_timeout` defaults to **false**. A timed-out command may have
  partially completed; automatic retry can duplicate destructive effects.
- Keep the name **`max_restart_attempts`**.
- Do **not** partition `runs` in v1. Add indexes and measure first.
- Store normalized definitions as **canonical JSON**. Every effective change,
  including a file-authority change, creates a revision. Do not retain a run's
  required revision for less time than the run itself.
- Start with **1 MiB log chunks**, measured before freezing. Live tail latency
  comes from the in-memory broadcaster, not the S3 object size.
- Keep `log_on_full` per definition with a daemon default. Use `drop_old` as
  the bounded-storage default, while changing the product promise from “full
  output” to “captured output up to the configured limit.”
- Do not add a hidden `trigger` CLI alias. One documented verb is better than
  hidden vocabulary.
- **Remove the npm/Bun download shim.** It adds a supply-chain and support
  channel that does not help the core ops audience.
- Webhook payloads carry **`"v": 1`** from the first release.
- Multiple/read-only network tokens remain post-v1 unless real use requires
  them.
- Use **Huma/handler-first OpenAPI** in Go, provided CI checks that the emitted
  document is stable. Streaming endpoints still need hand-written tests.
- Keep `run_on_start`; do not add `@reboot` or a cron year field.
- A Podman-compatible executable may be selected explicitly with a config key
  such as `container.runtime = "podman"`. Do not make an environment-only,
  silently detected choice.
- Keep prior DB revisions visible after an authority takeover.
- In system mode, unprivileged users do not see other namespaces, even as a
  names-only directory.
- Per-user quotas are **required before system mode ships**, not a later MAY.

## Proposed release scope

The accepted ADR for system mode can remain a product direction, but it should
not force a premature implementation. The root, multi-user API materially
changes the threat model.

### v0.1: core, local mode

Ship only:

- user mode on Linux and macOS;
- local jobs and one-instance local workers;
- five-field cron, descriptors, `@every`, and `catch_up = "none" | "latest"`;
- overlap `skip | parallel`;
- worker restart `always | on-failure | never` with fixed backoff;
- TOML bootstrap/includes and DB-native definitions;
- basic browser import as DB-native copy, TOML export, and linked-file reload;
- SQLite run/definition/audit state;
- local file logs, windowed reads, and authenticated live streaming;
- minimal REST API, Unix socket, one bearer token;
- dashboard, definition detail, run detail, log viewer, and one structured
  definition form;
- the essential CLI: `daemon`, `init`, `validate`, `list`, `run`, `logs`,
  `reload`, `import`, `export`, `status`, `token`, and `version`.

Defer from v0.1: queue/replace, job retries, worker instances/dependencies,
raw TOML dual-mode web editing, file watching, notifications, S3, Docker,
system mode, search, trigger parameters, service-file generation, and
backup commands. Unit/service templates can still be distributed as files.

### v0.2: advanced local operation

Add queue/replace, retries, worker instances, metrics, backup/restore,
notifications, typed trigger parameters, service installation, and optional
file watch. Add worker dependency ordering only if a concrete use case still
justifies it.

### v0.3: optional integrations

Add the Docker/Podman run driver and S3 logs. Both must be independently
optional at runtime while remaining compiled into the same binary.

### Later security milestone: system mode

Ship system mode only after its file-access, registration, authorization,
quota, and root-daemon design has an explicit security review. Calling this
v0.2 in the current roadmap is too aggressive.

## Required cross-spec corrections

### 1. Make reload atomic at the right boundary

The suite alternates between “reject the whole reload” and “reject one file
and apply all others.” Use this rule instead:

- In user mode, the bootstrap plus all of its includes form **one desired set**
  and apply atomically.
- In system mode, each owner scope is one independent desired set. One owner's
  invalid set does not block another owner, but files inside that owner's set
  do not partially apply.
- Duplicate-name and removed-definition checks run over the whole owner set.
- A DB transaction commits desired metadata before reconciliation. Process
  starts/stops are not transactionally atomic; reconciliation is idempotent
  and reports degraded items until converged.

This is understandable and avoids hybrid states assembled from individually
valid but collectively conflicting files.

### 2. Fix import semantics

A browser-uploaded file disappears after the HTTP request, so it cannot become
a linked, file-authoritative definition. Split import into explicit modes:

- `minicrond import --link PATH`: register a daemon-local path as
  file-authoritative.
- `minicrond import --copy PATH`: copy definitions into DB authority.
- Web/API upload: always **copy into DB authority** unless a future managed
  source-file feature is deliberately designed.

Preview/apply must bind to the exact uploaded bytes/hash to avoid a TOCTOU
change between preview and apply. `takeover` to file authority only makes
sense for `--link` on a daemon-local path.

### 3. Normalize run status semantics

Keep the existing terminal status vocabulary if desired, but make it
non-overlapping:

- `succeeded`: process exited with an allowed success code;
- `failed`: non-success exit, signal, start error, or log-overflow kill;
- `timeout`: deadline caused termination, regardless of the final signal or
  exit code;
- `stopped`: explicit operator/policy stop;
- `interrupted`: daemon recovery could not observe a final result;
- `skipped`: a known trigger was declined;
- `missed`: a scheduled occurrence was not run during downtime.

A success exit code must never turn a timeout into `succeeded`. Add
`scheduled_for`, `missed_count`, and structured trigger annotations rather
than hiding facts in system log lines. Specify whether `parent_run_id` points
at the previous attempt or root attempt; I recommend a separate
`retry_root_run_id` plus `attempt`.

### 4. Correct scheduler persistence and time behavior

- Use **UTC** as the default timezone everywhere. Never silently fall back from
  an invalid configured timezone; validation fails.
- Do not write a cron parser/evaluator from scratch. Put a mature Go cron
  parser behind a small internal schedule interface, then add conformance
  tests for the chosen semantics.
- `@every` must use a persisted activation/next-fire anchor. Anchoring it to
  daemon startup causes schedule drift on every restart and makes catch-up
  incoherent.
- Persist the schedule revision/hash with schedule state. A changed schedule
  must not create catch-up occurrences for time before that revision existed.
- Clarify DST without creating a storm. For a forward gap, coalesce matching
  fixed-time occurrences into at most one adjusted trigger at the end of the
  gap. During a repeated hour, execute a fixed wall-clock occurrence once.
  Record the adjustment in trigger metadata rather than creating many fake
  runs.
- Define a monotonic-time wakeup loop with wall-clock recomputation after
  material clock steps.
- Separate the active-job cap from long-running worker slots; workers must not
  consume all cron admission capacity indefinitely.

### 5. Correct process guarantees and Go feasibility

The current statement that `setsid` makes every descendant unable to escape is
false. A descendant can create a new session, and children may survive a
`kill -9` of the daemon. State the boundary honestly: process groups control
cooperative/trusted workloads; containers or future cgroups are the isolation
boundary for hostile workloads.

For Go specifically:

- `SysProcAttr.Credential`, process groups/sessions, and parent-death behavior
  cover the v0.1 core.
- Per-child umask and arbitrary rlimits are not safely implemented by changing
  process-global state around `os/exec` in a concurrent daemon. Either defer
  them or design an internal launcher mode using the same binary and a strict
  handshake. Do not silently race `umask` between runs.
- Persist PID, PGID, daemon boot ID, and process start identity as soon as the
  child starts. On restart, attempt to terminate a verified old process group
  before marking its run interrupted. Never trust a PID alone because of PID
  reuse.
- Change “all descendants die with the run” to “the daemon signals the run's
  process group; deliberate daemonization/session escape is unsupported.”
- Pure-Go static user lookup may not honor LDAP/SSSD/NSS. v1 should guarantee
  local passwd/group and numeric IDs; broader NSS support needs an explicit
  implementation and platform test.

### 6. Simplify and secure command execution

- Add an argv form early: exactly one of `command` (shell string) or `argv`
  (string array). It is safer and also maps naturally to Docker.
- Always execute shell commands as `shell -c <command>`. Do not change behavior
  based on whether the string contains a newline, do not create a root-owned
  temporary script that the run-as user cannot read, and do not silently add
  `-e` semantics.
- Default `env_base` to **`clean`**, not `inherit`. A root daemon's inherited
  AWS/cloud/service credentials must not flow into unprivileged jobs.
- Default `working_dir` to the effective user's home, with an explicit fallback
  if it is unavailable.
- Remove `MINICROND_LOG_PATH`; it leaks an internal path that the run-as user
  generally cannot access and encourages mutation of daemon-owned logs.
- `enabled = false` should prevent all new starts, including manual starts.
  Re-enable first; do not give the word “disabled” a partial meaning.
- Track a worker's operator stop/hold separately from crash exit, otherwise
  `restart = "always"` can immediately undo a stop request.

### 7. Replace global string substitution with typed references

Expanding `${VAR}` and `${file:path}` in almost every string is surprising,
can make definition identity depend on daemon environment, and becomes a root
file-read vulnerability in system mode.

Prefer:

- literal values by default;
- explicit environment references only where supported, for example
  `secret_env = { TOKEN = "env:BACKUP_TOKEN" }`;
- explicit file references, for example `"file:/run/secrets/token"`;
- no interpolation in names, commands, schedules, paths, or arbitrary config
  strings;
- absolute `env_file`/secret paths for DB-native definitions; relative paths
  may resolve against the source directory only for linked file definitions.

### 8. Repair the log format and promises

- “Exact interleaving” is impossible with independently drained stdout and
  stderr pipes. Promise a single sequence in **daemon ingestion order**.
- Define a real frame: version, sequence/line number, stream, timestamp,
  payload length, payload bytes, and flags. Specify handling of partial final
  lines, invalid UTF-8, CRLF, and oversized lines.
- zstd data uses `.zst` and `zstdcat`, not `.gz` and `zcat`.
- Live local tail should use the in-process broadcaster, not poll the file
  every 250 ms. Coordinate backlog and subscription by sequence number so no
  lines are lost between them.
- Do not `fsync` every small chunk at high output rates. Flush continuously
  but sync at bounded intervals and finalization; define the power-loss
  trade-off.
- ANSI rendering must allow only safe styling sequences. Strip/escape OSC,
  clipboard, title, and arbitrary terminal control sequences in the HTML
  renderer.
- S3 needs a bounded local spool/upload queue so retries never stop draining a
  child's pipes. Remote S3 health should be degraded state, not daemon
  unready, while the local spool still works.
- Do not allow static S3 secrets directly in TOML. Use the standard credential
  chain or explicit secret references.

### 9. Fix the SQLite model before migrations freeze it

The schema is a useful sketch but must not be copied directly into migration
001:

- `PRIMARY KEY (owner_user_id, name)` is unsafe with nullable
  `owner_user_id` in SQLite because NULL values do not provide the intended
  admin-name uniqueness. Use a surrogate `definition_id` plus separate
  partial unique indexes for admin and user scopes, or a non-null owner
  sentinel.
- `definition_revisions`, `schedule_state`, and `import_sources` must be keyed
  by definition/owner identity, not globally by bare name/path.
- Every file-authority effective change also increments a revision.
- Runs should carry `definition_id`, owner identity, definition hash,
  `scheduled_for`, retry root, boot ID, PGID/process identity, and missed
  count where applicable.
- Add storage for idempotency keys if the API promises them; scope keys by
  principal and operation and retain a request hash.
- Notification webhooks need a durable outbox/delivery table if retries are a
  product guarantee.
- Use one timestamp representation internally (integer Unix microseconds is
  simple); render RFC 3339 in APIs.
- A DB write actor may serialize writes, but lifecycle calls must await the
  commit acknowledgement before spawning or reporting success. “Async queue”
  must not mean fire-and-forget state transitions.
- Define retention combination precisely. I recommend deleting a terminal run
  when it exceeds either the count cap or age cap, while always retaining the
  newest terminal run.
- The current “soft-delete row, immediately delete logs, 24-hour undo” is not
  an undo. Either delay both row and log deletion or remove the undo claim. For
  automated retention, remove the undo claim.
- Direct read-only SQLite inspection can be documented as a debugging aid, but
  the schema is not a stable public API before v1 and direct writes are never
  supported.

### 10. Correct HTTP and SSE contracts

- Remove nonstandard HTTP **472**. A waited-for run that completed with an
  application failure is still a successfully handled HTTP request: return
  200 with `status = "failed"`; the CLI maps that result to an exit code.
- If `wait=true` reaches its HTTP wait limit while the run continues, return
  202 with the run ID and current state.
- Native browser EventSource cannot send a bearer header. Use `fetch()` plus
  a streaming SSE parser, or later add a carefully designed cookie session.
  Never put the bearer token in a query string.
- Per-connection SSE IDs are not reconnect-resumable. Log streams use stable
  line/sequence IDs. The global event stream needs daemon-boot-aware IDs and a
  `resync` event when replay is unavailable; clients then refetch state.
- Add optimistic concurrency to definition edits (`ETag`/revision plus
  `If-Match`) so two tabs cannot silently overwrite each other.
- Define PATCH semantics, or prefer full `PUT` replacement initially.
- Scope `Idempotency-Key` by authenticated principal and target. Reusing a key
  with different content returns 409.
- Do not represent system namespaces as `alice.backup-db`; dots are already
  legal in definition names. Use an unambiguous `owner/name` CLI form and
  explicit owner path/query fields in the API.
- Disable CORS by default. Do not trust proxy forwarding headers unless a
  trusted-proxy setting explicitly enables them.

### 11. Reduce UI surface

Remove or defer these ideas:

- “copy as curl everywhere” — it adds visual noise and risks copying a token;
  provide a developer menu on relevant detail/action views and use
  `$MINICROND_TOKEN` placeholders;
- side-by-side form and TOML editors — ship one structured editor first;
- progressive enhancement/server-rendered navigation — incompatible with the
  selected embedded static SPA unless a second rendering implementation is
  built;
- “disabled jobs that fired nothing” in Needs Attention — disabled jobs are
  normally intentional;
- displaying the current auth token in Settings — allow rotation and show a
  fingerprint, never recover/display the existing secret.

Retain the dashboard, definition page, run page, and log viewer. Those are the
product.

### 12. Strengthen root and system-mode security

System mode has unresolved privilege-escalation paths in the current draft:

- A root daemon must not read a user's linked config, `env_file`, or
  `secret_env` target with root authority and then pass the bytes to that
  user. Otherwise a user can reference `/etc/shadow` or cloud credentials.
  Reads must be performed with owner-equivalent permissions using a reviewed
  helper/open protocol, with symlink and path-race behavior specified.
- Self-registration conflicts with “registration is the gate.” If every local
  user can call `user register`, it is not an administrator-controlled gate.
  Choose admin registration: `sudo minicrond user add <user>`, then grant
  socket group/ACL access. Registered users may rotate their own token.
- Do not fall back to a world-writable system socket. Configure the group/ACL
  correctly or fail installation with a useful error.
- Add per-owner limits for definitions, active job runs, worker slots, queued
  runs, log bytes/rate, SSE clients, and imports before system mode ships.
- User-configurable webhook URLs or S3 endpoints would create root-daemon SSRF.
  Channels and storage endpoints remain admin-owned; definitions only select
  predeclared channel names.
- System mode should initially be Linux-only unless peer credentials,
  privilege dropping, service installation, and path access are separately
  tested on macOS.

### 13. Simplify Docker scope

- Keep one `driver = "docker"` that starts a new labeled container for a job
  or foreground worker. The daemon owns only containers bearing its instance
  and run labels.
- The first implementation may invoke an explicitly configured Docker/Podman
  CLI; document that this optional integration has that runtime dependency.
  “One binary” describes minicrond's distribution, not the Docker runtime.
- Rename the proposed `:dind` image; it contains a client, not a Docker daemon.
  Better: one normal image plus a documented way to add/mount the client, or a
  `:docker-cli` variant.
- Do not claim a distroless image can execute arbitrary local shell jobs; it
  has no `/bin/sh`. Either use a small base with a shell or document that the
  image supports container-driver jobs only.
- Drop Compose service reconciliation and Compose import from the committed
  roadmap until the simple driver is proven.

### 14. Make notifications safe and durable

- Never include the last log lines in outbound webhooks by default; logs often
  contain secrets and customer data.
- Start with one versioned JSON body. Do not add arbitrary template files in
  the first notification release.
- Store pending deliveries and retry state durably in SQLite. An in-memory
  retry loop does not support the stated delivery guarantee across restart.
- Coalescing should key by owner plus event kind plus definition, not bare job
  name.
- Delivery failures create inbox events without recursively notifying the
  same failing route.

### 15. Correct operational claims

- Replace Rust-specific `tracing`, `tokio`, and binary-size text with Go's
  `slog`, goroutines, and measured Go budgets.
- Treat `<25 MB` binary and `<30 MB` RSS as benchmark goals, not hard product
  requirements, until measured with SQLite, the embedded UI, zstd, and the S3
  SDK compiled in.
- Under systemd/launchd, log the daemon to stderr and let the service manager
  rotate it. Do not also write `daemon.log` by default and build another
  rotation system.
- `/readyz` means the DB, scheduler, and local log ingestion path can accept
  work. A temporary remote S3 failure should report degradation through
  status/metrics without causing a restart loop when a local spool is usable.
- Remove the npm/Bun installation channel.

## Acceptance criteria to add before implementation

The prose should be backed by a small normative test matrix:

1. **Run transitions:** every allowed transition and every forbidden
   transition, including start error, timeout, stop, crash recovery, and
   retry-chain final status.
2. **Reload:** valid update, one invalid owner set, removed definition,
   missing source, file move with unchanged spec, and authority conflict.
3. **Scheduler:** DST gap/fold, daemon downtime, schedule revision change,
   `@every` across restart, clock jumps, overlap skip, and global capacity.
4. **Process control:** run-as UID/GID/groups, clean environment, process
   group stop, timeout escalation, daemon crash/restart cleanup, and PID reuse
   protection.
5. **Logs:** partial lines, invalid UTF-8, oversized records, stdout/stderr
   ingestion ordering, cap policies, crash-truncated chunk, backlog-to-live
   handoff, SSE resume, and S3 outage with a full upload queue.
6. **Security:** browser stream auth, token rotation, CSRF/CORS assumptions,
   linked-file permissions, user secret-file denial, namespace isolation,
   webhook SSRF boundaries, and ZIP traversal if ZIP import is ever added.
7. **SQLite migration:** fresh DB, upgrade, newer-schema refusal, backup while
   active, retention with failed log deletion, and nullable-owner uniqueness.

## Final product opinion

Build the excellent small local scheduler/supervisor first. Do not let
multi-user root tenancy, Compose reconciliation, notification templating, or
distribution experiments turn it into a workflow platform before the core is
trustworthy. The durable differentiator is simple run history and logs for
ordinary commands, not the number of integrations in the roadmap.
