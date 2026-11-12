# Embedding minicrond's UI behind a gateway

A gateway such as [Bento](https://github.com/khanhicetea/bento) can vendor the
**built** `internal/api/assets/` directory from a minicrond release tag, serve
the SPA itself, and proxy only `/api/v1/*` to the daemon's private Unix socket.
Never serve HTML or executable assets supplied by that socket: a compromised
app can replace the daemon. All API responses, including errors, names, log
output, IDs and URLs, are untrusted data even over a local socket.

## Static bundle contract

- `index.html` contains exactly one literal `<base href="/" />`.
- Its only script and stylesheet entries are `src="./app-<hash>.js"` and
  `href="./style-<hash>.css"`. There is one JS bundle and one stylesheet,
  alongside `index.html`; no inline scripts/styles or remote entry links.
- Serve those files at `<BASE_PATH>/assets/<filename>`. Serve `index.html`
  for client-side routes, not for missing assets or API paths.
- Apply the same literal rewrites as `Server.ui`:
  1. `<base href="/" />` → `<base href="<BASE_PATH>/" />` (once).
  2. `src="./` → `src="assets/` (all).
  3. `href="./` → `href="assets/` (all).
- `BASE_PATH` denotes the URL prefix (configured on the daemon with
  `MINICROND_BASE_PATH`); it is an empty string at the origin root, otherwise a validated
  absolute path prefix without a trailing slash (e.g. `/tools/minicron`).
  Use a trailing slash in the HTML base. Redirect the bare prefix to its
  slash-terminated form. Do not accept arbitrary HTML in this value.
- The SPA derives its routing/API prefix from that base tag. Every UI data
  request, including its token-free daemon probe, downloads, and SSE, uses
  `<BASE_PATH>/api/v1/*`. It does **not** need `/healthz`, `/readyz`, or
  `/openapi.json`. Built-in editor schemas ship in the bundle.

The gateway may strip its public prefix before forwarding to a daemon with
empty `BASE_PATH`, or preserve it when the daemon has the same `BASE_PATH`.
Do not apply the prefix twice. Vendor the UI and daemon from the same release;
this is a bundle layout contract, not a promise of cross-version API support.
**Changes to BASE_PATH semantics or asset naming require a release note**, so
gateways can update their rewrites before upgrading.

Releases also publish `minicrond-web-<tag>.tar.gz` and its `.sha256`. The archive
contains the contents of `internal/api/assets/` at its root (`./index.html`,
`./app-*.js`, `./style-*.css`). It packages the checked-in build without
rebuilding it: entries are sorted, timestamps are zero, numeric owner/group
are zero, and gzip omits its filename/timestamp. Verify the checksum before
vendoring, and obtain the archive/checksum from a trusted release source.

## Embedded (iframe) layout

The SPA switches to a compact layout when it runs inside another page's
iframe (detected with `window.self !== window.top`) or when it is opened with
`?embed=1` (remembered for the browser tab). It sets `data-embed="true"` on
`<html>`: the header drops the product name and collapses search to an icon,
gains an "Open in a new tab" button, and the three Runs panes narrow so the
log keeps most of a ~1000 px frame. Nothing about the bundle contract or the
API changes. Framing still needs the daemon/gateway to allow it
(`frame-ancestors`, see below).

## Theming

Colors, radii, type sizes and control heights are CSS custom properties in
`web/src/styles/tokens.css` (`--mc-*`, with dark and light sets). Components
and pages use them through the shared classes in `web/src/index.css` and the
Tailwind aliases declared there (`bg-surface`, `text-danger-fg`, …); reusable
React pieces live in `web/src/components/ui/`. To rebrand, change the tokens
and rebuild the bundle.

## API media types

All `/api/v1/*` response bodies, including errors and router redirects, stay
within these media types (parameters such as `charset=utf-8` are allowed):

| Media type | Use |
|---|---|
| `application/json` | Structured results and errors; JSON exports |
| `text/plain` | Raw log downloads; router-generated responses |
| `text/event-stream` | Resumable SSE log streams |
| `application/toml` | TOML exports |

Bento parses `Content-Type` and rejects anything outside this allowlist with
**502**, rather than forwarding executable HTML, JavaScript, or SVG. Keep
`X-Content-Type-Options: nosniff`. This API-only allowlist does not apply to
the gateway's trusted static HTML/JS/CSS. Allow SSE streaming without response
buffering; preserve event IDs/resume semantics and cancellation.

## UI trust-boundary rules

- Render API strings only as escaped text/React children. Never use
  `dangerouslySetInnerHTML`, `innerHTML`, or equivalent HTML sinks. This also
  applies to error bodies and ANSI/log output; do not interpret log hyperlinks
  or escape sequences as HTML. Review third-party rendering too (the chart
  serializer has an API-label escaping regression test).
- Never assign an API-provided URL directly to `href` or `src`. If external
  API URLs are introduced, parse them with `new URL` and allow **only** the
  resulting `http:`/`https:` protocols before rendering; do not rely on a
  string prefix check. The UI currently renders no such external URLs.
  Internal routes must use a code-owned local prefix and encode API-derived
  path/query fields (`jobPath`, `encodeURIComponent`), not treat them as URLs.
- Download filenames are fixed code-owned literals, never API fields or
  upstream `Content-Disposition`. Download bodies go through locally created
  blob URLs, not API-provided `href`/`src`. Blob URLs are the deliberate local
  download exception to the external-URL scheme rule; revoke them after use.
- `apiPath` confines UI fetches to the prefixed `/api/v1/` namespace, including
  after browser URL normalization. Keep networking centralized in `api.ts`.

`go test ./internal/api` pins the shipped HTML and representative API response
types. `cd web && npm test` checks source networking, bans HTML sinks, guards
URL/filename rendering patterns, and tests malicious labels and URL escapes.
The source checks are conservative guardrails, **not full taint analysis**;
new rendering patterns still require security review. Regenerate the checked-in
bundle with `make build-web` after changing UI code.

The gateway still owns authentication, authorization and CSRF protection for
**every** request, including static files, downloads and SSE. Treat proxy
access as full command-execution/operator access, deny token rotation, and
isolate daemons by origin. Serve trusted static files with a restrictive CSP;
when framing, restrict `frame-ancestors` to the intended parent. See the
[API authentication and framing requirements](http-api.md#authentication).
