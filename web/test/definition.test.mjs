import assert from 'node:assert/strict';
import { test } from 'node:test';
import { duplicateDefinition, emptyDefinition, updateDefinitionDraft } from '../src/lib/definition.ts';

test('switching a new job to a worker clears job-only fields before save', () => {
  const job = { ...emptyDefinition('job'), name: 'ticker', command: 'sleep 10' };
  const worker = updateDefinitionDraft(job, { kind: 'worker' });

  assert.equal(worker.name, 'ticker');
  assert.equal(worker.kind, 'worker');
  assert.equal(worker.schedule, undefined);
  assert.equal(worker.retries, undefined);
  assert.equal(worker.retry_delay, undefined);
  assert.equal(worker.catch_up, undefined);
  assert.equal(worker.on_overlap, undefined);
  assert.equal(worker.restart, 'always');
});

test('switching back to a job restores job defaults without worker fields', () => {
  const worker = { ...emptyDefinition('worker'), name: 'ticker' };
  const job = updateDefinitionDraft(worker, { kind: 'job' });

  assert.equal(job.schedule, '0 0 * * *');
  assert.equal(job.autostart, undefined);
  assert.equal(job.restart, undefined);
});

test('duplicating a worker does not invent a schedule', () => {
  const worker = { ...emptyDefinition('worker'), name: 'ticker', command: 'sleep 10', revision: 2, definition_id: 7 };
  const copy = duplicateDefinition(worker);

  assert.equal(copy.name, 'ticker-copy');
  assert.equal(copy.schedule, undefined);
  assert.equal(copy.revision, undefined);
  assert.equal(copy.definition_id, undefined);
});
