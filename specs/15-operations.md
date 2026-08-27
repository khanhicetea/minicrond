# 15 — Operations

Status: Draft

## Installation & autostart

- **Install**: one-line script, Homebrew tap, GitHub tarballs, npm-style
  `bunx minicrond` shim (optional, downloads native binary), Docker images
  (`10`). Binary lands on PATH; nothing else.
- **`minicrond service install` [v0.2]**:
  - Linux: systemd unit — system (`/etc/systemd/system/minicrond.service`)
    when root, or `--user` (`~/.config/systemd/user/` + `loginctl
    enable-linger`) when not. System installs also create the `minicrond`
    group and the `/run/minicrond` socket dir for system mode (`17`).
  - macOS: launchd plist `~/Library/LaunchAgents/dev.minicrond.plist`
    (`--user` only).
  - Generated files carry a `# managed by minicrond service install` header
    + config hash; drift (header edited) prompts unless `--force`.
    `service uninstall` keeps data; `--purge` deletes the data dir behind a
    typed literal confirmation.
  - `service status`, `service print` (stdout preview) companions.
- **Upgrades**: replace binary, restart daemon. SQLite migrations run
  forward-only at boot; a *newer schema than binary* is a hard error with
  version guidance (downgrade = restore backup). Run history survives
  upgrades by default.

## Metrics (Prometheus text at `/metrics`)

| Metric | Type | Labels |
|---|---|---|
| `minicrond_runs_total` | counter | job, status |
| `minicrond_run_duration_seconds` | histogram | job |
| `minicrond_active_runs` | gauge | — |
| `minicrond_job_queue_depth` | gauge | job |
| `minicrond_next_fire_timestamp_seconds` | gauge | job (0 for workers) |
| `minicrond_worker_instances` | gauge | worker, state |
| `minicrond_daemon_memory_bytes` / `cpu_seconds` | gauge | — |
| `minicrond_log_sink_errors_total` | counter | backend |
| `minicrond_config_stale` | gauge | — |
| `minicrond_db_wal_bytes` | gauge | — |

Health: `/healthz` (process alive), `/readyz` (scheduler loaded + db writable
+ log sink writable).

## Resource envelope (targets, verified in CI benchmarks)

- Idle RSS < 30 MB; startup < 1 s; binary < 25 MB (Rust, musl, release +
  strip + LTO).
- Scheduler tick cost < 1 ms at 1k definitions.
- Log pump overhead < 2% CPU at 10 MB/s throughput (worst-case verbose
  worker).

## Daemon's own logging

- `tracing` structured logs to stderr (foreground) or `daemon.log` +
  stderr (service mode); `MINICROND_LOG_FORMAT=json` for collectors;
  `MINICROND_LOG_LEVEL` (default `info`).
- Rotation of `daemon.log`: `SIGHUP`-safe; recommend journald/logrotate in
  docs; internal size cap (default 50 MB, rotate ×3) as a net regardless.

## Backup / restore [v0.2]

`minicrond backup` = `VACUUM INTO` snapshot + manifest (+ optional logs tar
for the file backend; S3 prefix listing otherwise) — see `08`. Restore
is documented file replacement. A daily self-backup is **not** scheduled by
default (we don't silently write extra copies); docs show the three-line
job definition that does it — the product eating its own dog food.

## Troubleshooting story

- `minicrond doctor` (env checks), `/api/v1/daemon` (capabilities),
  config-state endpoint (staleness), `runs` table as public data, daemon log
  tail in Settings. Common runbook entries in docs: second-instance lock,
  newer-schema error, token rotation, S3 connectivity for the log backend.
- Crash dumps: panic handler logs stack to `daemon.log` and exits 1;
  systemd `Restart=on-failure` covers the loop.

## Documentation

- Docs site (static, from the repo): quickstart, config reference
  (generated from the JSON Schema), API explorer (published OpenAPI),
  runbooks, recipes. Versioned per release.
- `minicrond doctor --explain` prints doc deep-links for each finding.

## Open questions

- OQ-19 metrics placement (recommend: same port, auth'd).
- npm/bun distribution shim: worth the surface area? (Recommendation: yes —
  the reference product validated the demand channel — but v0.2, after the
  install script solidifies.)
