import assert from 'node:assert/strict';
import { test } from 'node:test';
import { humanizeSchedule } from '../src/lib/cron.ts';

test('every-minute cron is not described as hourly', () => {
  assert.equal(humanizeSchedule('* * * * *'), 'Every minute');
});

test('hourly and stepped schedules keep their descriptions', () => {
  assert.equal(humanizeSchedule('0 * * * *'), 'Every hour at :00');
  assert.equal(humanizeSchedule('@hourly'), 'Every hour at :00');
  assert.equal(humanizeSchedule('*/5 * * * *'), 'Every 5m');
  assert.equal(humanizeSchedule('57 14 * * *'), 'Daily at 14:57');
  assert.equal(humanizeSchedule('@every 90s'), 'Every 90s');
});
