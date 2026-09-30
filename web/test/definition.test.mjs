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

  assert.equal(copy.name, '');
  assert.equal(copy.schedule, undefined);
  assert.equal(copy.revision, undefined);
  assert.equal(copy.definition_id, undefined);
});

test('cloning preserves all job settings while clearing identity and runtime metadata', () => {
  const settings = {
    ...emptyDefinition('job'),
    enabled: false,
    command: 'echo startup',
    schedule: '',
    run_on_start: true,
    timezone: 'Asia/Ho_Chi_Minh',
    retries: 3,
    env: { REGION: 'local' },
    secret_env: { TOKEN: 'example' },
    labels: { description: 'startup task' },
    alerts: ['ops'],
    success_codes: [0, 2],
    keep_runs: 12,
  };
  const original = {
    ...settings,
    name: 'startup',
    revision: 3,
    definition_id: 8,
    source: 'config',
    next_fire_at: '2026-10-01T00:00:00Z',
  };
  const copy = duplicateDefinition(original);

  assert.deepEqual(copy, { ...settings, name: '' });
  copy.env.REGION = 'changed';
  copy.secret_env.TOKEN = 'changed';
  copy.labels.description = 'changed';
  copy.alerts.push('other');
  copy.success_codes.push(3);
  assert.equal(original.env.REGION, 'local');
  assert.equal(original.secret_env.TOKEN, 'example');
  assert.equal(original.labels.description, 'startup task');
  assert.deepEqual(original.alerts, ['ops']);
  assert.deepEqual(original.success_codes, [0, 2]);
  assert.equal(original.name, 'startup');
});

test('cloning preserves worker execution and restart settings', () => {
  const worker = {
    ...emptyDefinition('worker'),
    name: 'queue',
    argv: ['queue', '--consume'],
    autostart: false,
    restart: 'on-failure',
    restart_delay: 15,
    max_restart_attempts: 10,
    healthy_after: 60,
    working_dir: '/srv/queue',
    timeout: 120,
    log_max: 25,
  };
  const copy = duplicateDefinition(worker);

  assert.deepEqual(copy, { ...worker, name: '' });
  copy.argv.push('--verbose');
  assert.deepEqual(worker.argv, ['queue', '--consume']);
});
