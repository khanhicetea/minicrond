# `minicron` name check (pre-release)

Date: 2026-08-28. Per OQ-11 the name was frozen subject to a package/domain/trademark check before the first public release.

## Findings

| Check | Result |
|---|---|
| Go module path `github.com/minicron/minicron` | unclaimed on proxy.golang.org / pkg.go.dev (404) — **but** the `github.com/minicron` organization is owned by someone else, so this path is not publishable as-is |
| Existing Go packages | **collision**: an unrelated `minicron/client...` module exists on pkg.go.dev |
| Homebrew formula | API unreachable from the build environment; unverified |
| Domains | not verifiable from the build environment; `minicron.<tld>` availability must be checked manually |
| Trademarks | USPTO/EUIPO search must be done manually; no automated verdict |

## Recommendation

- Keep the binary/product name `minicron` (decision OQ-11) — no evidence of a conflicting product in this category was found.
- **Move the Go module to a namespace the project owns** (e.g. `github.com/<owner>/minicron`) before the first public release; the current `github.com/minicron/minicron` path in `go.mod` is a placeholder that cannot be published under the taken organization.
- Complete the domain and trademark checks manually before signing release artifacts; the v0.1.0 release notes must not ship until they are recorded here.
