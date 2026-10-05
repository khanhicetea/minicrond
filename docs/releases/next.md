# minicrond (next release): log disk budget

The log disk budget (ADR-8 3A, [ADR-10](../adr/0010-log-disk-budget.md)) is new and
its headroom rule is **on by default**.

- `logs.disk_min_free` defaults to **512 MiB** (clamped to a quarter of the
  filesystem). When free space on the data directory's filesystem falls below it,
  the oldest completed runs' logs are deleted early, ahead of `keep_for` /
  `logs.db_keep_for`, until free space reaches 125% of it. Deleted logs are not
  recoverable; run records and other metadata are never touched.
- To keep the previous behavior set `logs.disk_min_free = 0`. To bound log disk use
  directly set `logs.disk_budget` (MiB, default off).
- New opt-in quarantine purge: `logs.quarantine_keep_for`, `logs.quarantine_max_size`.
- `GET /api/v1/daemon` gains `diagnostics.log_storage` and
  `diagnostics.terminal_persistence`.
- The daemon logs a `log disk budget:` INFO line at start and reload with the
  effective settings.
- Admission of new work is not affected by disk pressure; when reclamation is
  insufficient, log output is dropped under the capture-failure policy.
