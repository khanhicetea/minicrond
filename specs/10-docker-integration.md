# 10 — Docker Integration

Status: Draft · Scope/phasing: OQ-14 · Nice-to-have per D-3 — nothing here
blocks v0.1.

## Three pairing modes

### Mode A — minicron in a container (the managed box)

Run the official image; it schedules/supervises whatever lives inside its own
container or talks to sibling containers via a mounted socket (Mode B in the
same breath).

```bash
docker run -d --name minicron \
  -p 7423:7423 \
  -v minicron-data:/var/lib/minicron \
  -v ./minicron.toml:/etc/minicron/minicron.toml:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \   # enables Mode B
  minicron/minicron:latest
```

- Image variants: `minicron` (static binary, distroless-ish minimal) and
  `minicron:dind` (+ docker CLI & compose plugin, for Modes B/C).
- The daemon runs as root **in the image** specifically to support per-job
  `run_as` (D-5) and socket use; hardening notes in `13`.
- Healthcheck: `minicron healthcheck` (hits `/healthz` internally).
- Data volume at `/var/lib/minicron`; config mounted read-only works because
  imports read files, the registry lives in the volume (spec `05` — a mounted
  read-only config + writable registry is a first-class combination).

### Mode B — socket sidecar (manage sibling containers)

The daemon on the host (or the privileged sidecar) supervises **other
containers' services** by driving the Docker API over the socket:

- `[[worker]]` with `driver = "docker_compose_service"` [v0.3] references a
  compose project + service; minicron wraps `docker compose up -d <svc>` /
  `stop`, surfaces container status and `docker logs -f` through the normal
  run/log pipeline (same UI as local workers).
- Managed resources are **labeled** (`dev.minicron.managed=<daemon
  instance_id>`) so the daemon never touches containers it didn't create,
  and can reclaim stale labeled leftovers after a crash.

### Mode C — docker driver for jobs (run *in* containers)

`driver = "docker"` on a `[[job]]` (spec `07`):

```toml
[[job]]
name     = "db-migrate"
schedule = "@daily"
driver   = "docker"
image    = "migrate/migrate"
command  = "-path /migrations -database postgres://... up"   # container args
volumes  = ["./migrations:/migrations:ro"]                   # bind mounts
env      = { ... }
```

- Executor runs: `docker run --rm --init --name minicron-<job>-<run8>`,
  streams output (stdout/stderr demuxed via docker's multiplexed stream)
  into the LogSink, records the container exit code as the run's exit code.
- Stop ladder maps to `docker stop -t <grace>` then `docker kill`.
- Privilege notes: `run_as` does not apply (container UID mapping instead:
  `user = "1000:1000"` passed through as docker `--user`).

**Implementation stance**: drive the Docker/Podman **CLI** rather than the
API SDK — full feature parity (compose especially), trivially works with
rootless podman via `DOCKER_HOST`, and the CLI's stdout formats are stable
contracts. Availability probed lazily; a missing docker runtime fails the run
with `start_error` + a `docker.unavailable` event.

## Compose import [v0.3]

`minicron import --from compose docker-compose.yml` generates `[[worker]]`
definitions (driver Mode B) per service: image, command, env, and a mapping
of compose `restart:` policies onto our restart semantics; anything without
a clean mapping gets a `# TODO` comment. Import is **generative** — the
compose file remains yours; we don't rewrite it.

## Anti-goals

- Not a container runtime, orchestrator, or k8s anything.
- No image building features beyond passing `dockerfile:` through to
  `docker build` in v1.0+ if demanded.

## Security notes (detailed in `13`)

- Docker socket = root on the host. Mode B docs MUST carry a warning banner
  and the socket path must never be auto-discovered silently — it is
  explicit configuration only.
- Images are pulled by the daemon only when a job says so (`pull = "missing" |
  "always"`); no implicit `latest` surprise on reload.

## Open questions

- OQ-14: keep Mode C (job driver) in v0.2 and defer Mode B (compose service
  supervision) + compose import to v0.3? Recommendation: yes — C is the
  high-value/low-surface piece.
- Podman parity: accept `CONTAINER_CLI=podman` env override from day one?
  (Cheap with the CLI stance; recommendation yes.)
