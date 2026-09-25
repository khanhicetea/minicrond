# ADR-7: Unix-only transport and proxied UI

- Date: 2026-09-25
- Status: accepted

## Decision

The daemon keeps TCP and bearer-token login as the standalone default. An
optional `server.tcp_enabled = false` mode serves the full API, SSE, assets,
and SPA only on the private Unix socket. At least one listener is required.
Unix-only mode does not issue or rotate bearer tokens, and its daemon status
does not expose a token fingerprint. An existing token hash remains stored
for a later return to TCP mode. Listener changes require restart.

The browser first probes a protected endpoint without a token. A 401 opens
the usual TCP token login. A successful probe opens token-free UI access
only when the daemon reports TCP disabled. Proxy sessions never send a
retained browser token. A later 401 ends the session and clears cached UI
data. The daemon continues to deny direct framing.

The supported browser proxy arrangement uses Unix-only mode. The external
proxy owns session authentication, per-daemon authorization, CSRF/Origin
checks, browser origin isolation, streaming, and narrow framing policy.
It must deny token rotation. Access to the API is full operator access,
including execution of arbitrary definitions as the daemon UID. The Unix
socket continues to trust only same-UID and root kernel peers.

## Consequences

Independent local and container deployments can avoid a TCP port and run
under numeric UIDs without a passwd entry. CLI and registry ownership stay
unchanged. The browser UI has two distinct authentication states, while
minicrond gains no external identity headers, registry ownership protocol,
or container-runtime dependency.
