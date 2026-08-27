# 02 — Technology Choices

Status: Draft (revised post-ADR-1) · Decisions: OQ-1 (language), OQ-9 (UI
framework), OQ-12 (license)

## Language decision (OQ-1)

ADR-1 dropped SlateDB — which was the only constraint pinning us to Rust.
What still discriminates between candidates:

- **D-5 per-run privilege dropping** (`setuid`/`setgid`/`setgroups` before
  exec): native in Go and Rust, effectively impossible in Bun → **Bun stays
  disqualified**.
- **D-1 single static binary** with embedded UI: Go and Rust both fine.
- SQLite (D-2), SSE, S3 client, cron/timezones, docker CLI control: both
  fine.

| Criterion | **Go (recommended)** | Rust | compiled Bun |
|---|---|---|---|
| SQLite | ✅ `modernc.org/sqlite` (pure Go → CGO off) | ✅ `rusqlite` (bundled) | ✅ `bun:sqlite` |
| Static binary | ✅ `CGO_ENABLED=0` (~15–25 MB) | ✅ musl (~10–20 MB) | ⚠️ ~90 MB self-extractor |
| Privilege dropping (D-5) | ✅ `syscall`/`x/sys/unix`, `SysProcAttr{Setpgid, Pdeathsig}` | ✅ `nix` crate | ❌ deal-breaker |
| Web + SSE | ✅ std `net/http` | ✅ `axum` + `tokio` | ✅ |
| Cron & timezones | ✅ own AST + stdlib `time/tzdata` | ✅ `jiff` | ⚠️ |
| Docker/Podman | ✅ SDK or CLI (CLI chosen, `10`) | ⚠️ CLI | ⚠️ |
| S3 client | ✅ `aws-sdk-go-v2` / `minio-go` | ✅ `object_store` | ⚠️ |
| Dev velocity / contributor pool | ✅ fastest | ⚠️ slower | ✅ |
| Memory floor | ⚠️ ~15–25 MB idle | ✅ ~5–15 MB idle | ⚠️ |

**Recommendation: Go.** With log offload reduced to plain object I/O
(ADR-1), this is an ops tool where iteration speed, ecosystem batteries
(cron semantics to borrow, `fsnotify`, docker clients, embedded tzdata), and
trivial cross-compilation outweigh Rust's tighter footprint. Rust remains
the documented alternative if we need memory below Go's floor or want
stronger type-system guarantees around the concurrent executor — a defensible
veto, just no longer forced by a dependency.

## Go stack picks (if OQ-1 accepts Go)

| Concern | Pick | Notes |
|---|---|---|
| HTTP + routing + SSE | std `net/http` (+ `chi` if middleware grows) | SSE = `http.Flusher`; no framework weight |
| SQLite | `modernc.org/sqlite` | pure Go, static builds; `mattn/go-sqlite3` only if cgo perf ever needed |
| Migrations | `golang-migrate` or hand-rolled `user_version` stepper | forward-only |
| IDs | `github.com/google/uuid` `NewV7()` | run ids, time-sortable |
| Timezones | stdlib `import _ "time/tzdata"` | bundled IANA data, zero deps |
| Config | `pelletier/go-toml/v2`, strict decode | errors carry positions |
| File watching | `fsnotify` + debounce | opt-in (OQ-13) |
| Compression | `klauspost/compress/zstd` | log chunks |
| S3 | `aws-sdk-go-v2/service/s3` (custom endpoint) | MinIO/R2 via endpoint + path style |
| CLI | `urfave/cli/v3` | completions, JSON flags |
| Embedded UI | std `embed.FS` | hashed assets, SPA fallback route |
| OpenAPI | `huma` (handler-first) or `oapi-codegen` (spec-first) | decide at kickoff |
| Structured logging | std `log/slog` | `text`/`json` handlers built in |
| Process supervision | std `os/exec` + `x/sys/unix` | process groups, signals, setuid |

Rust alternative (condensed, for the veto path): `tokio` + `axum` +
`rusqlite(bundled)` + `jiff` + `object_store` + `zstd` + `clap` +
`rust-embed` — all prior analysis holds except the SlateDB row, which is
gone.

## Web UI stack (OQ-9)

Requirements: embedded static SPA (no server-side JS), small bundle, good
DX, SSE-friendly, TypeScript.

**Recommendation: Svelte 5 + Vite**, static build (`adapter-static`), ~50–150 KB
gzip. Alternatives: SolidJS (equal fit), Preact (smallest, fewer ergonomics).
No server framework; the daemon serves `index.html` + hashed assets and a
catch-all route; data over the REST/SSE API only.

## Build & release

- Go: `CGO_ENABLED=0`, `GOOS`/`GOARCH` matrix — `linux/amd64`,
  `linux/arm64`, `darwin/amd64`, `darwin/arm64` (all static, one command
  each). Rust path (if chosen): musl + osxcross/CI runners.
- Release channels: GitHub Releases tarballs (+ checksums), one-line install
  script, Homebrew tap, Docker images `minicron` and `minicron:dind`
  (docker-cli variant) — see `10`.
- Versioning: semver; before 1.0 breaking changes allowed but MUST ship a
  config migration note; config vocabulary is frozen at v0.1 regardless
  (goal, not promise — but renames require an ADR).

## License (OQ-12)

**Recommendation: Apache-2.0** (explicit patent grant, one license for
everything, matches the ecosystem's norm). Alternative: dual MIT/Apache-2.0.
We are not deriving from GPL code; `learn/` is reference-only and gitignored
(ADR-0).

## Open questions

- OQ-1: Go (recommended post-ADR-1) vs Rust veto?
- OQ-9: Svelte 5 (recommended) vs SolidJS vs Preact.
- OpenAPI style if Go: `huma` (handler-first, like the reference product's
  approach) vs `oapi-codegen` (spec-first — the spec `11` doc then becomes
  generative input). Lean `huma`; decide at kickoff.
