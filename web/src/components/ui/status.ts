import type { IconName } from '../Icon';

/** Semantic tone shared by badges, dots, strips and callouts (see styles/tokens.css). */
export type Tone = 'ok' | 'danger' | 'warn' | 'info' | 'neutral';

interface RunStatusMeta {
  label: string;
  tone: Tone;
  icon: IconName;
  spin?: boolean;
}

const RUN_STATUS: Record<string, RunStatusMeta> = {
  succeeded: { label: 'Succeeded', tone: 'ok', icon: 'circle-check' },
  failed: { label: 'Failed', tone: 'danger', icon: 'circle-x' },
  timeout: { label: 'Timeout', tone: 'danger', icon: 'alert-circle' },
  interrupted: { label: 'Interrupted', tone: 'danger', icon: 'alert-triangle' },
  stopped: { label: 'Stopped', tone: 'warn', icon: 'square' },
  skipped: { label: 'Skipped', tone: 'warn', icon: 'pause' },
  missed: { label: 'Missed', tone: 'warn', icon: 'alert-triangle' },
  running: { label: 'Running', tone: 'info', icon: 'loader', spin: true },
  queued: { label: 'Queued', tone: 'info', icon: 'clock' },
  pending: { label: 'Pending', tone: 'info', icon: 'clock' },
};

export function runStatusMeta(status: string): RunStatusMeta {
  return RUN_STATUS[status] ?? { label: status, tone: 'neutral', icon: 'info' };
}

export function isFailedStatus(status: string): boolean {
  return status === 'failed' || status === 'timeout' || status === 'interrupted';
}

/** CSS class names per tone, kept here so components never hard-code colors. */
export const TONE_CHIP: Record<Tone, string> = {
  ok: 'chip-success',
  danger: 'chip-error',
  warn: 'chip-warn',
  info: 'chip-info',
  neutral: 'chip-neutral',
};

export const TONE_DOT: Record<Tone, string> = {
  ok: 'dot-green',
  danger: 'dot-red',
  warn: 'dot-amber',
  info: 'dot-blue',
  neutral: 'dot-gray',
};
