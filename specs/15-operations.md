# 15 — Operations

Status: Draft

## Installation & autostart

- **Install**: one-line script, Homebrew tap, GitHub tarballs, npm-style
  `bunx minicron` shim (optional, downloads native binary), Docker images
  (`10`). Binary lands on PATH; nothing else.
- **`minicron service install` [v0.2]**:
  - Linux: systemd unit — system (`/etc/systemd/system/minicron.service`)
    when root, or `--user` (`~/.config/systemd/user/` + `loginctl
    enable-linger`) when not. System installs also create the `minicron`
    group and the `/run/minicron` socket dir for system mode (`17`).
  - Generated files carry a `# managed by minicron service install` header
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
| `minicron_runs_total` | counter | job, status |
| `minicron_run_duration_seconds` | histogram | job |
| `minicron_active_runs` | gauge | — |
| `minicron_job_queue_depth` | gauge | job |
| `minicron_next_fire_timestamp_seconds` | gauge | job (0 for workers) |
| `minicron_worker_instances` | gauge | worker, state |
| `minicron_daemon_memory_bytes` / `cpu_seconds` | gauge | — |
| `minicron_log_sink_errors_total` | counter | backend |
| `minicron_config_stale` | gauge | — |
| `minicron_db_wal_bytes` | gauge | — |

Health: `/healthz` (process alive), `/readyz` (scheduler loaded + db writable
+ log sink writable).

## Resource envelope (targets, verified in CI benchmarks)

- Idle RSS < 30 MB; startup < 1 s; binary < 25 MB are measured Go build
  goals, not contractual limits.
- Scheduler tick cost < 1 ms at 1k definitions.
- Log pump overhead < 2% CPU at 10 MB/s throughput (worst-case verbose
  worker).

## Daemon's own logging

- Go `slog` structured logs go to stderr; systemd owns rotation; `MINICRON_LOG_FORMAT=json` for collectors;
  `MINICRON_LOG_LEVEL` (default `info`).
- Rotation of `daemon.log`: `SIGHUP`-safe; recommend journald/logrotate in
  docs; internal size cap (default 50 MB, rotate ×3) as a net regardless.

## Backup / restore [v0.2]

`minicron backup` = `VACUUM INTO` snapshot + manifest (+ optional logs tar
for the file backend; S3 prefix listing otherwise) — see `08`. Restore
is documented file replacement. A daily self-backup is **not** scheduled by
default (we don't silently write extra copies); docs show the three-line
job definition that does it — the product eating its own dog food.

## Troubleshooting story

- `minicron doctor` (env checks), `/api/v1/daemon` (capabilities),
  config-state endpoint (staleness), `runs` table as public data, daemon log
  tail in Settings. Common runbook entries in docs: second-instance lock,
  newer-schema error, token rotation, S3 connectivity for the log backend.
- Crash dumps: panic handler logs stack to `daemon.log` and exits 1;
  systemd `Restart=on-failure` covers the loop.

## Documentation

- Docs site (static, from the repo): quickstart, config reference
  (generated from the JSON Schema), API explorer (published OpenAPI),
  runbooks, recipes. Versioned per release.
- `minicron doctor --explain` prints doc deep-links for each finding.

## Open questions

- OQ-19 metrics placement (recommend: same port, auth'd).
- npm/bun distribution shim: worth the surface area? (Recommendation: yes —
  the reference product validated the demand channel — but v0.2, after the
  install script solidifies.)
