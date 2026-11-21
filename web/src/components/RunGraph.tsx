import { runStatusMeta, isFailedStatus, type Tone } from './ui';
import type { Run } from '../types';

/** Bars in each job's run graph. */
const GRAPH_SLOTS = 30;

export const TONE_VAR: Record<Tone, string> = {
  ok: 'var(--mc-ok)',
  danger: 'var(--mc-danger)',
  warn: 'var(--mc-warn)',
  info: 'var(--mc-info)',
  neutral: 'var(--mc-neutral)',
};

export function durationMs(run: Run, now: number): number {
  const start = Date.parse(run.started_at ?? run.queued_at);
  const end = run.ended_at ? Date.parse(run.ended_at) : now;
  return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, end - start) : 0;
}

/**
 * One bar per recent run, oldest on the left: height is the run's duration
 * (square-root scale, so one long run does not flatten the rest), color its status.
 */
export function RunGraph({ runs, now }: { runs: Run[]; now: number }) {
  const recent = runs.slice(0, GRAPH_SLOTS).reverse();
  const longest = Math.sqrt(Math.max(1, ...recent.map(run => durationMs(run, now))));
  const failed = recent.filter(run => isFailedStatus(run.status)).length;
  const label = recent.length === 0 ? 'No recent runs' : `Last ${recent.length} runs: ${failed} failed`;
  return (
    <span role="img" aria-label={label} title={label} className="run-graph">
      {Array.from({ length: GRAPH_SLOTS - recent.length }, (_, index) => (
        <span key={`empty-${index}`} className="empty" />
      ))}
      {recent.map(run => (
        <span key={run.run_id} style={{ height: `${Math.round(4 + (14 * Math.sqrt(durationMs(run, now))) / longest)}px`, background: TONE_VAR[runStatusMeta(run.status).tone] }} />
      ))}
    </span>
  );
}

