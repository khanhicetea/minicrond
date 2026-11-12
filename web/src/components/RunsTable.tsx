import { Link } from 'wouter';
import { jobPath } from '../lib/routes';
import { Icon } from './Icon';
import StatusBadge from './StatusBadge';
import { Dot, EmptyState, buttonClass, runStatusMeta } from './ui';
import type { Run } from '../types';
import { isActiveRun } from '../types';
import { formatIsoLocal, formatSpan } from '../lib/format';

/** Max rows rendered in compact mode (definition run history). */
export const COMPACT_LIMIT = 12;

/**
 * Recent-runs table: started-at, job, status, duration, trigger, and a link to
 * the run. With `bare`, the outer card is omitted (the host supplies its own).
 */
export default function RunsTable({ runs, compact = false, bare = false }: { runs: Run[]; compact?: boolean; bare?: boolean }) {
  if (runs.length === 0) {
    const empty = <EmptyState icon="history" title="No runs yet." />;
    return bare ? empty : <div className="table-wrap">{empty}</div>;
  }
  const visible = compact ? runs.slice(0, COMPACT_LIMIT) : runs;
  const table = (
    <>
      <table className="mc-table min-w-[36rem]">
        <thead>
          <tr>
            <th scope="col">
              <span className="inline-flex items-center gap-1">
                Started at <Icon name="arrow-down" size={12} />
              </span>
            </th>
            <th scope="col">Job</th>
            <th scope="col">Status</th>
            <th scope="col">Duration</th>
            <th scope="col">Trigger</th>
            <th scope="col" className="text-right">
              Log
            </th>
          </tr>
        </thead>
        <tbody>
          {visible.map(run => (
            <tr key={run.run_id}>
              <td className="whitespace-nowrap num">{formatIsoLocal(run.started_at ?? run.queued_at)}</td>
              <td>
                <Link href={jobPath(run.job)} className="font-mono text-[0.8rem] text-link hover:text-link-hover">
                  {run.job}
                </Link>
              </td>
              <td>
                <StatusBadge status={run.status} />
              </td>
              <td className="num whitespace-nowrap">{isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at)}</td>
              <td className="muted">{run.trigger}</td>
              <td className="text-right">
                <Link href={`/runs/${encodeURIComponent(run.run_id)}`} className={buttonClass('secondary', 'sm')} aria-label={`View log for ${run.job}`}>
                  View log
                </Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {compact && !bare && runs.length > visible.length && (
        <div className="border-t border-line px-4 py-2 text-center">
          <Link href="/runs" className="text-xs text-link hover:text-link-hover">
            View all runs
          </Link>
        </div>
      )}
    </>
  );
  return bare ? <div className="overflow-x-auto">{table}</div> : <div className="table-wrap">{table}</div>;
}

/** Status dot used in compact run lists. */
export function RunStatusDot({ status }: { status: string }) {
  const meta = runStatusMeta(status);
  return <Dot tone={meta.tone} pulse={meta.spin} className="mt-[0.3rem]" />;
}
