import { isActiveRun, type Run } from '../../types';
import { isFailedStatus } from './status';

function slotClass(run: Pick<Run, 'status'> | undefined): string {
  if (!run) return 's-empty';
  if (run.status === 'succeeded') return 's-ok';
  if (isFailedStatus(run.status)) return 's-fail';
  if (isActiveRun(run)) return 's-run';
  return 's-warn';
}

/**
 * One bar per recent run, oldest on the left. Failures are taller so they read
 * by height, not hue alone. `runs` is newest-first, as the API returns them.
 */
export function RunStrip({ runs, slots = 20, size = 'sm', className = '' }: { runs: Pick<Run, 'status'>[]; slots?: number; size?: 'sm' | 'lg'; className?: string }) {
  const recent = runs.slice(0, slots);
  const failed = recent.filter(run => isFailedStatus(run.status)).length;
  const padded: (Pick<Run, 'status'> | undefined)[] = [...Array(Math.max(0, slots - recent.length)).fill(undefined), ...[...recent].reverse()];
  const label = recent.length === 0 ? 'No recent runs' : `Last ${recent.length} runs: ${failed} failed`;
  return (
    <span role="img" aria-label={label} title={label} className={`run-strip ${size === 'lg' ? 'run-strip-lg' : ''} ${className}`}>
      {padded.map((run, index) => (
        <span key={index} className={slotClass(run)} />
      ))}
    </span>
  );
}

export function failedCount(runs: Pick<Run, 'status'>[], slots = 20): number {
  return runs.slice(0, slots).filter(run => isFailedStatus(run.status)).length;
}
