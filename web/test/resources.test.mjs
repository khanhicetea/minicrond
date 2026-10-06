import assert from 'node:assert/strict';
import { test } from 'node:test';
import { cpuPercent, memoryMiB } from '../src/lib/resources.ts';

const previous = { sampled_at: '2026-01-01T00:00:00Z', process_start_id: '123', cpu_us: 1_000_000, rss_bytes: 1024 };
const current = { ...previous, sampled_at: '2026-01-01T00:00:03Z', cpu_us: 2_500_000 };
test('interval CPU uses one logical CPU as 100%', () => {
  assert.equal(cpuPercent(previous, current), 50);
  assert.equal(cpuPercent(previous, { ...current, cpu_us: 7_000_000 }), 200);
  assert.equal(cpuPercent(previous, { ...current, cpu_us: previous.cpu_us }), 0);
});
test('missing, reused, reset and stale samples are not fabricated zero CPU', () => {
  assert.equal(cpuPercent(undefined, current), undefined);
  assert.equal(cpuPercent(previous, null), undefined);
  assert.equal(cpuPercent(previous, { ...current, process_start_id: '456' }), undefined);
  assert.equal(cpuPercent(previous, { ...current, cpu_us: 0 }), undefined);
  assert.equal(cpuPercent(previous, previous), undefined);
  assert.equal(cpuPercent(previous, { ...current, sampled_at: 'invalid' }), undefined);
});
test('RSS display distinguishes unavailable from zero', () => {
  assert.equal(memoryMiB(null), '—');
  assert.equal(memoryMiB(0), '0.0 MiB');
  assert.equal(memoryMiB(1024 * 1024), '1.0 MiB');
});
