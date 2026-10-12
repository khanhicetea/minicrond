# ADR-15: opt-in administrator-controlled executable search paths

## Context and decision

Bare argv lookup previously ignored inherited PATH and searched only /usr/bin and
/bin. That safe default fails in common PHP/Node/Python containers that install
root-controlled toolchains in /usr/local/bin. Preserve the default, but allow an
ordered bootstrap-only `scheduler.trusted_path` list (1–32 absolute directories).
Changing it requires restart, keeping admitted/replayed definitions independent
of mutable per-definition search policies.

Using ambient or definition PATH was rejected: those values may include writable
application directories and should not become executable trust policy. Checking
only the final canonical directory was also rejected: a symlink through a writable
intermediate directory could be retargeted after validation.

## Security and ownership

Configured directories, each traversed ancestor/symlink route, and resolved files
must be root-owned without group/other write permission. Checking ownership, not
just present permissions, rejects a user-owned 0555 directory that the job could
chmod. The POSIX ACL mask is reflected in group mode bits on supported Linux
filesystems, so granting a named non-root user write access also fails validation.
Root is the trusted administrator, as elsewhere in minicrond's trusted-workload
model; this is not a sandbox for root jobs or processes granted root-equivalent
capabilities. The stricter root-ownership policy intentionally rejects toolchains
owned by a different non-root account too, rather than adding per-identity/ACL
permission simulation or dropping privileges in helper processes.

Validation walks path components, following at most 40 symlinks per resolution.
It runs at bootstrap and on each spawn, with no cache or background goroutine.
The immutable executor options copy owns the path list. No process-global PATH
mutation occurs. Bare names use only that list; absolute executables retain their
existing explicit semantics and relative paths with slashes remain rejected.
Configured directories also supply the clean environment's initial PATH; inherited
and explicit PATH values retain precedence for shell/subprocess lookup. Credential
selection, run_as checks, and numeric-UID HOME rules are unchanged.

## Costs, evidence, and revisit conditions

The opt-in path incurs bounded-count filesystem validation at spawn, not on log
capture or with idle workers. Unsafe or unavailable paths fail configuration or
produce a normal start_error if they change after bootstrap; supervision applies
its usual crash budget. This favors security and predictable unattended restarts
over shaving startup stat calls. Default lookup remains unchanged.

Tests cover safe system symlinks, writable routes with safe canonical targets,
ownership/mode/ACL-mask rejection, path limits, configured lookup independent of
ambient PATH, clean/inherited/explicit environment precedence, absolute paths,
non-root run_as rejection, and restart-required configuration changes. No startup
performance gain or power-loss behavior is claimed. Revisit only if deployments
need a more permissive explicitly specified non-root trust authority, not by
silently accepting user-writable PATH entries.
