# 12 — Web UI

Status: Draft · Framework: OQ-9 · Constraint D-6: simple, DX-friendly

## Principles

1. **Zero-friction first run**: binary → open `http://localhost:7423` →
   enter token → see the dashboard. No onboarding wizard, no account.
2. **Every screen answers an operator question** in this order: *is it
   healthy? → what ran? → why did it fail? → what did it print?* No screen
   exists for any other purpose. If a feature doesn't serve those four, it's
   out.
3. **Curl-able**: every UI view has "copy as curl" for the data it shows;
   every action button has "copy as curl" for the call it makes. The UI
   teaches the API as a side effect.
4. **Fast on a Raspberry Pi over SSH-tunneled 3G**: minimal JS (< 150 KB
   gzip), no webfonts, no CDN, server-rendered-ish shell (HTML + hydration
   optional), dark/light/auto.
5. **Live by default**: dashboards and run pages update via one shared SSE
   connection (leader-tab election not needed v1 — one connection per tab is
   fine at this scale; document and revisit only if profiling says so).

## Pages

### Dashboard (`/`)
- Health strip: daemon up/down, version, scheduler timezone, and registry status.
- "Needs attention": failed runs (last 24 h), fatal workers, disabled jobs
  that fired nothing in 7 days.
- Active runs (live status chips + jump to log).
- Up next: next 5 firings across jobs (with countdowns).
- Recent runs table (20) with status, duration, trigger icons.

### Jobs & Workers (`/jobs`)
- One list, kind column + filter chips (kind, label, enabled, health).
- Row: name (with owner badge in system mode, `17`), humanized schedule
  ("every night 2:00 AM +07"), next fire,
  last status + duration, active count/instances, enable toggle, kebab menu
  (trigger / stop instances / edit / export / duplicate / delete).
- Sort by name/next-fire/last-status. Search by name.

### Job detail (`/jobs/{name}`)
- Summary card: schedule (raw + humanized + tz), next 5 firings, overlap &
  retry policy, `run_as`, revision, and owner badge (system mode, `17`),
  enabled toggle, definition revision + history drawer (audit entries).
- History: run list with filters (status/trigger/time), each row → run page.
- Actions: **Run now** (opens params form when the job declares params
  [v0.2]), Stop, Enable/Disable.
- For workers: instance cards (1..N) with per-instance status, uptime,
  restart count, restart/stop buttons, fatal banner with "Restart now".

### Run detail (`/runs/{id}`)
- Header: status chip, exit code / signal, start/end/duration, trigger,
  attempt chain (linked retries), `run_as`, log size + truncation badge.
- **Log viewer** (the heart of the product):
  - virtualized rendering, ANSI colors parsed, wrap toggle,
  - stream filter tabs: all / stdout / stderr / system (server-side tagged
    lines make this free),
  - live-follow with auto-scroll (pauses on manual scroll-up, "resume"
    pill), backlog seeded (last 1000 lines) then SSE,
  - jump-to-line, copy line/range, download raw,
  - search within run [v0.2].
- Retry chain strip: attempt 1 → 2 → 3 with statuses.

### Editor (`/jobs/{name}/edit`, `/jobs/new`)
- Every definition is editable through the same registry-backed editor.
- Form mode (structured fields with inline docs) **and** TOML source mode
  (textarea with validation) side by side; either saves through the same
  `PATCH /api/v1/jobs` path. Server-side validation errors map back to
  fields (or line/col in TOML mode).
- Cron helper: builder dropdowns + live "next 5 firings" preview from the
  real scheduler AST — the preview cannot lie.
- Secrets fields render as `•••` with "reference only" hints; values never
  round-trip.
- Save = new revision + audit; "diff against current" preview before apply.

### Import / Export (`/settings/io`)
- Export panel (scope pickers, format, revision metadata, secrets
  stripped notice) and Import panel (paste/upload → validation → diff table
  → conflict resolution → apply → summary) — the exact flow of spec `05`,
  since the UI is a client of those endpoints.

### Settings (`/settings`)
- Auth token display/rotate, retention overview, storage/log backend status
  (incl. S3 log storage health when enabled), definition-registry status,
  daemon log tail (last 200 lines, SSE-follow).

## Non-goals in the UI

- No dashboard-builder/graphing; link to `/metrics` for Prometheus users.
- No mobile app / native wrappers; the responsive web page is the story.
- No multi-user management v1 (single admin token; RBAC is a v2 question).

## Accessibility & keyboard

- Standard focus order, visible focus, contrast-checked palette.
- `j/k` navigate rows, `Enter` open, `r` trigger run (with confirm),
  `/` search, `?` shortcuts overlay. Server-rendered links work without JS
  for navigation (progressive enhancement where cheap).

## Open questions

- OQ-9: resolved — React 19 (React Compiler) + wouter + daisyUI 5 +
  TanStack Query; bundle-size and DX baselines were close, the deciding
  factors were ecosystem familiarity and compiler-driven memoization.
- ~~TUI~~ — decided: **no TUI, ever** (ADR-2). The web UI + CLI are the only
  interfaces; SSH port-forwarding covers the headless case.
- Web UI namespace scoping for system-mode users (`17`) is v1.1; v1 UI is
  admin-token with owner badges.
