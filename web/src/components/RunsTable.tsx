import { Link } from 'wouter';
import { jobPath } from '../lib/routes';
import { Icon } from './Icon';
import StatusBadge from './StatusBadge';
import type { Run } from '../types';
import { isActiveRun } from '../types';
import { formatDayTime, formatIsoLocal, formatSpan, shortId } from '../lib/format';

/** Max rows rendered in compact mode (dashboard). */
export const COMPACT_LIMIT = 12;

/**
 * Recent-runs table from the reference design: started-at, job, status,
 * duration, trigger, and a "View log" action. With `bare`, the outer panel
 * is omitted (the host supplies its own panel + header).
 */
export default function RunsTable({ runs, compact = false, bare = false }: { runs: Run[]; compact?: boolean; bare?: boolean }) {
  if (runs.length === 0) {
    const empty = (
      <div className="px-6 py-10 text-center">
        <Icon name="history" size={24} className="mx-auto faint" />
        <p className="mt-2 text-sm muted">No runs yet.</p>
      </div>
    );
    return bare ? empty : <div className="panel overflow-x-auto">{empty}</div>;
  }
  const visible = compact ? runs.slice(0, COMPACT_LIMIT) : runs;
  const table = (
    <>
      <table className="mc-table min-w-[36rem]">
        <thead>
          <tr>
            <th>
              <span className="inline-flex items-center gap-1">
                Started at <Icon name="arrow-down" size={12} />
              </span>
            </th>
            <th>Job</th>
            <th>Status</th>
            <th>Duration</th>
            <th>Trigger</th>
            <th className="text-right">Log</th>
          </tr>
        </thead>
        <tbody>
          {visible.map(run => (
            <tr key={run.run_id}>
              <td className="whitespace-nowrap num">{formatIsoLocal(run.started_at ?? run.queued_at)}</td>
              <td>
                <Link
                  href={jobPath(run.job)}
                  className="font-mono text-[0.8rem] text-sky-300 hover:text-sky-200"
                >
                  {run.job}
                </Link>
              </td>
              <td>
                <StatusBadge status={run.status} />
              </td>
              <td className="num whitespace-nowrap">
                {isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at)}
              </td>
              <td className="muted">{run.trigger}</td>
              <td className="text-right">
                <Link
                  href={`/runs/${run.run_id}`}
                  className="btn-sub !px-2 !py-1.5"
                  title="View log"
                  aria-label={`View log for ${run.job}`}
                >
                  <Icon name="external-link" size={13} />
                </Link>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {compact && !bare && runs.length > visible.length && (
        <div className="border-t border-base-300 px-4 py-2 text-center">
          <Link href="/runs" className="text-xs text-sky-300 hover:text-sky-200">
            View all runs
          </Link>
        </div>
      )}
    </>
  );
  return bare ? (
    <div className="overflow-x-auto">{table}</div>
  ) : (
    <div className="panel overflow-x-auto">{table}</div>
  );
}

/** Sidebar-style run list used on the run detail page. */
export function RunsList({ runs, currentId }: { runs: Run[]; currentId?: string }) {
  if (runs.length === 0) {
    return (
      <p className="rounded-xl border border-dashed border-base-300 px-3 py-6 text-center text-sm faint">
        No runs match this filter.
      </p>
    );
  }
  return (
    <ol className="space-y-1">
      {runs.map(run => {
        const current = run.run_id === currentId;
        return (
          <li key={run.run_id}>
            <Link
              href={`/runs/${run.run_id}`}
              aria-current={current ? 'page' : undefined}
              className={`group flex items-start gap-2.5 rounded-xl border px-3 py-2.5 transition-colors ${
                current
                  ? 'border-blue-500/60 bg-blue-500/10'
                  : 'border-transparent hover:border-base-300 hover:bg-base-300/30'
              }`}
            >
              <span className="mt-0.5">
                <RunStatusDot status={run.status} />
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex items-baseline gap-2">
                  <span className="font-mono text-[0.8125rem] font-semibold">#{shortId(run.run_id, 8)}</span>
                  <span className="ml-auto whitespace-nowrap text-xs muted">
                    {formatDayTime(run.started_at ?? run.queued_at)}
                  </span>
                </div>
                <div className="mt-1 flex items-center gap-1.5 text-xs">
                  <span className={statusTextClass(run.status)}>{statusLabel(run.status)}</span>
                  <span className="faint" aria-hidden>·</span>
                  <span className="muted capitalize">{run.trigger}</span>
                  <span className="faint" aria-hidden>·</span>
                  <span className="num muted">
                    {isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at)}
                  </span>
                  <Icon
                    name="chevron-right"
                    size={13}
                    className="ml-auto shrink-0 faint opacity-0 transition-opacity group-hover:opacity-100"
                  />
                </div>
              </div>
            </Link>
          </li>
        );
      })}
    </ol>
  );
}

/** Human label for a run status. */
function statusLabel(status: string): string {
  const labels: Record<string, string> = {
    succeeded: 'Success',
    failed: 'Failed',
    timeout: 'Timeout',
    interrupted: 'Interrupted',
    stopped: 'Stopped',
    skipped: 'Skipped',
    missed: 'Missed',
    running: 'Running',
    pending: 'Pending',
  };
  return labels[status] ?? status;
}

/** Text color matching a run status. */
function statusTextClass(status: string): string {
  if (status === 'succeeded') return 'text-green-400';
  if (status === 'failed' || status === 'timeout' || status === 'interrupted') return 'text-red-400';
  if (status === 'running' || status === 'pending') return 'text-blue-400';
  return 'text-amber-400';
}

/** Round status marker used in compact run lists. */
export function RunStatusDot({ status }: { status: string }) {
  if (status === 'succeeded')
    return <span className="inline-flex h-4 w-4 items-center justify-center rounded-full bg-green-500/15 text-green-400"><Icon name="check" size={10} strokeWidth={3} /></span>;
  if (status === 'failed' || status === 'timeout')
    return <span className="inline-flex h-4 w-4 items-center justify-center rounded-full bg-red-500/15 text-red-400"><Icon name="alert-circle" size={10} /></span>;
  if (status === 'running' || status === 'pending')
    return <span className="inline-flex h-4 w-4 items-center justify-center rounded-full bg-blue-500/15 text-blue-400"><Icon name="loader" size={10} className="spin" /></span>;
  return <span className="inline-flex h-4 w-4 items-center justify-center rounded-full bg-amber-500/15 text-amber-400"><Icon name="clock" size={10} /></span>;
}
