import { isValidTimezone, parseSchedule } from './cron';
import type { Definition } from '../types';

export interface DefinitionIssue {
  field: string;
  line?: number;
  title: string;
  message: string;
  severity: 'error' | 'hint';
}

/**
 * Client-side validation mirroring the daemon's config rules plus a few
 * editorial hints. Runs on every keystroke in the definition editor so the
 * TOML panel can show live line-numbered issues.
 */
export function validateDefinition(def: Definition, lineOf: Record<string, number> = {}): DefinitionIssue[] {
  const issues: DefinitionIssue[] = [];
  const add = (issue: DefinitionIssue) => issues.push({ ...issue, line: issue.line ?? lineOf[issue.field] });
  const isWorker = def.kind === 'worker';

  // Name
  if (!def.name) {
    add({ field: 'name', title: 'Missing name', message: 'Every definition needs a unique name.', severity: 'error' });
  } else if (!/^[a-z0-9][a-z0-9_.\-]{0,99}$/.test(def.name)) {
    add({
      field: 'name',
      title: 'Invalid: Name',
      message: 'Use lowercase letters, digits, dots, dashes or underscores (start alphanumeric).',
      severity: 'error',
    });
  }

  // Exactly one of command / argv.
  const hasCommand = Boolean(def.command && def.command.trim());
  const hasArgv = Boolean(def.argv && def.argv.length > 0);
  if (!hasCommand && !hasArgv) {
    add({ field: 'command', title: 'Missing command', message: 'Set exactly one of command (shell) or argv.', severity: 'error' });
  } else if (hasCommand && hasArgv) {
    add({ field: 'command', title: 'Invalid: Command', message: 'command and argv are mutually exclusive; keep one.', severity: 'error' });
  }

  if (!isWorker) {
    if ((!def.schedule || !def.schedule.trim()) && !(def.source === 'config' && def.run_on_start)) {
      add({ field: 'schedule', title: 'Missing schedule', message: 'Jobs need a cron expression or an @every interval.', severity: 'error' });
    } else if (def.schedule && !parseSchedule(def.schedule)) {
      add({
        field: 'schedule',
        title: 'Invalid: Cron expression',
        message: 'Value must have 5 fields (minute hour day month day-of-week) or be "@every <duration>".',
        severity: 'error',
      });
    }
  }

  if (def.timezone && !isValidTimezone(def.timezone)) {
    add({ field: 'timezone', title: 'Invalid: Timezone', message: `Unknown IANA timezone "${def.timezone}".`, severity: 'error' });
  }

  for (const [field, label, allowZero] of [
    ['timeout', 'Timeout', true],
    ['grace', 'Grace', false],
    ['restart_delay', 'Restart delay', false],
    ['retry_delay', 'Retry delay', true],
    ['healthy_after', 'Healthy after', false],
    ['keep_for', 'Keep for', true],
    ['log_max', 'Log max size (MiB)', true],
  ] as const) {
    const value = def[field];
    if (typeof value === 'number' && (!Number.isInteger(value) || value < 0 || (!allowZero && value === 0))) {
      add({ field, title: `Invalid: ${label}`, message: `Must be ${allowZero ? 'a non-negative' : 'a positive'} whole number.`, severity: 'error' });
    }
  }

  if (def.retry_delay !== undefined && def.retry_delay > 86400) {
    add({ field: 'retry_delay', title: 'Invalid: Retry delay', message: 'Must not exceed 86400 seconds.', severity: 'error' });
  }
  if (def.retries !== undefined && (!Number.isInteger(def.retries) || def.retries < 0 || def.retries > 1000)) {
    add({ field: 'retries', title: 'Invalid: Retries', message: 'Must be between 0 and 1000.', severity: 'error' });
  }
  if (isWorker && (def.retries || def.retry_delay !== undefined)) {
    add({ field: 'retries', title: 'Invalid: Worker retry', message: 'Use restart policy for workers.', severity: 'error' });
  }

  if (def.log_max !== undefined && def.log_max > 1048576) {
    add({ field: 'log_max', title: 'Invalid: Log max size', message: 'Must not exceed 1048576 MiB (1 TiB).', severity: 'error' });
  }

  for (const field of ['keep_runs', 'max_restart_attempts', 'priority'] as const) {
    const value = def[field];
    if (typeof value === 'number' && (!Number.isInteger(value) || value < 0)) {
      add({ field, title: 'Invalid: Number', message: 'Must be a non-negative integer.', severity: 'error' });
    }
  }

  if (def.success_codes && def.success_codes.some(code => !Number.isInteger(code) || code < 0 || code > 255)) {
    add({ field: 'success_codes', title: 'Invalid: Success codes', message: 'Exit codes must be integers between 0 and 255.', severity: 'error' });
  }

  // Hints.
  const commandText = `${def.command ?? ''} ${(def.argv ?? []).join(' ')}`.toLowerCase();
  if (hasCommand && !def.timeout && /backup|dump|export|archive|sync|restore/.test(commandText)) {
    add({
      field: 'timeout',
      title: 'Hint: Command timeout',
      message: 'Consider setting a timeout for long-running backup-style commands.',
      severity: 'hint',
    });
  }
  if (!isWorker && def.on_overlap === 'parallel' && !def.timeout) {
    add({
      field: 'on_overlap',
      title: 'Hint: Overlap policy',
      message: 'Parallel overlap without a timeout can accumulate runs; consider a timeout.',
      severity: 'hint',
    });
  }
  if (!isWorker && def.catch_up === 'latest' && def.run_on_start) {
    add({
      field: 'catch_up',
      title: 'Hint: Catch-up',
      message: 'run_on_start plus catch_up "latest" triggers once at daemon start; disable one if unintended.',
      severity: 'hint',
    });
  }

  return issues;
}
