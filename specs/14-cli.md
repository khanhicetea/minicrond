# 14 — CLI

Status: Draft

The binary **is** the CLI and the daemon (`minicron` with no subcommand =
foreground daemon; everything else is a subcommand). Local commands prefer
the Unix socket (peer-auth, zero config); `--url`/`minicron_URL` +
`minicron_TOKEN` point them at a remote daemon (same API the UI uses).

## Command surface (v0.1)

| Command | Behavior | Exit codes |
|---|---|---|
| `minicron` | foreground daemon (logs to stderr) | 0 clean shutdown; 1 fatal |
| `minicron daemon` | headless daemon; missing config = hard error (no prompts) | as above |
| `minicron init` | scaffold `minicron.toml` + example jobs dir (detects crontabs/compose files and mentions them) | 0/1 |
| `minicron run <name> [--wait] [-- param args…]` | trigger a run; `--wait` blocks, streams log to stderr, prints run JSON on stdout; propagates the run's exit code | run's exit code; 2 = trigger rejected |
| `minicron logs <run_id> [--follow] [--tail N]` | view a run's log (windowed or live); local socket or remote | 0/1 |
| `minicron list [kind] [--json]` | table of jobs/workers with status + next fire | 0/1 |
| `minicron status` | daemon status one-glance (version, uptime, active runs, staleness) | 0 running/1 not |
| `minicron reload` | config reload; prints diff summary (added/removed/changed) or validation errors | 0/1 |
| `minicron validate [path]` | dry-run config load; `--json` positional errors | 0 valid/1 invalid |
| `minicron schema` | print JSON Schema of the config | 0 |
| `minicron export […]` | see `05`: `--kind --label --format toml\|json` | 0/1 |
| `minicron import <path>…` | see `05`: `--dry-run --strategy` | 0/1 |
| `minicron token [--rotate]` | print/rotate auth token (local only) | 0/1 |
| `minicron stop` | graceful daemon stop (service-aware: systemd/launchd when managed, else SIGTERM to PID from lock) | 0/1 |
| `minicron doctor` | env diagnosis: config found? data dir writable? socket alive? docker present? disk headroom? clock sane? exit-code driven for scripts | 0 ok/1 warn/2 fail |
| `minicron version` | version, commit, build target | 0 |

[v0.2]: `minicron backup`, `minicron import --from crontab`,
`minicron user register|whoami|list|token|unregister` (system mode —
`17`), `minicron service install|uninstall|status` (see `15`).

## Conventions

- **`--json` on every read command** (stable, documented shapes — the API's
  shapes, re-used); human output is decorative, JSON is the contract.
- Exit codes mean something: `0` success, `1` operational failure,
  `2` usage/validation error, run-triggering commands propagate run exit
  codes (CI-friendly).
- `minicron_CONFIG`, `minicron_DATA`, `minicron_URL`, `minicron_TOKEN`,
  `minicron_LOG_LEVEL` env overrides; flags beat env beat defaults.
- No interactive prompts outside `init`/first-run and destructive confirms
  (`delete`, `token --rotate`); every prompt has `--yes` for scripts.
- In system mode, every command operates on the caller's scope (peer-uid
  identity — listing shows status, next fire, last run status; `logs` views
  run output); admin adds `--all` to see every scope (`17`).
- Shell completion generation: `minicron completion <shell>`.

## Scripting examples (docs-grade)

```bash
# CI: run the migration job and fail the pipeline if it fails
minicron run db-migrate --wait && echo migrated

# Cron-out: dump config for review
minicron export --format toml > /tmp/all.toml

# Health probe in a watchdog
minicron status --json | jq -e '.running'

# One-liner remote trigger
minicron_URL=https://box:7423 minicron_TOKEN=… minicron run backup-db
```

## Open questions

- Subcommand naming: `run` vs `trigger` (recommendation `run` — verb matches
  mental model, `trigger` stays the API word for the action resource). Accept
  `trigger` as alias? Recommendation: yes, hidden alias.
- ~~`minicron logs -f` in v0.1?~~ Promoted to v0.1 — CLI run-log viewing is
  a product requirement (system-mode users are CLI-first, `17`).
