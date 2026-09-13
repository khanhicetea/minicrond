# `minicrond` name check (pre-release)

Date: 2026-08-28. The Go module and command were renamed before the first public release; the existing `minicron` configuration, environment, storage, and API identifiers remain compatibility names.

## Findings

| Check | Result |
|---|---|
| Go module path `github.com/khanhicetea/minicrond` | owned project namespace; this is the module path used by `go.mod` |
| Existing Go packages | the `minicrond` module path avoids the unrelated `minicron/client...` package collision |
| Homebrew formula | API unreachable from the build environment; unverified |
| Domains | not verifiable from the build environment; `minicrond.<tld>` availability must be checked manually |
| Trademarks | USPTO/EUIPO search must be done manually; no automated verdict |

## Decision

- Use `github.com/khanhicetea/minicrond` as the Go module path and `minicrond` as the command name.
- Retain `minicron`-prefixed runtime identifiers for compatibility with existing configurations and data directories.
- Complete the domain and trademark checks manually before signing release artifacts; the v0.1.0 release notes must not ship until they are recorded here.
