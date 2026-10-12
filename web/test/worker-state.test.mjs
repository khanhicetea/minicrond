import assert from 'node:assert/strict';
import { test } from 'node:test';
import { workerStatusLabel } from '../src/lib/worker-state.ts';

test('exhaustion is explicit, and a retry counter alone is not fatal', () => {
  const state = { held: false, active: false, failures: 2, status: 'restarting' };
  assert.equal(workerStatusLabel(state), 'Restarting');
  assert.equal(workerStatusLabel({ ...state, status: 'exhausted' }), 'Exhausted');
  assert.equal(workerStatusLabel({ ...state, status: 'exhausted', exhaustion_pending: true }), 'Exhausted (persistence pending)');
  assert.equal(workerStatusLabel({ ...state, active: true, status: 'running', failures: 0 }), 'Running');
});
