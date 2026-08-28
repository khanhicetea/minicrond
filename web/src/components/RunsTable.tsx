import { Link } from 'wouter';
import type { Run } from '../types';
import { isActiveRun } from '../types';
import { formatSpan, formatTimestamp, shortId } from '../lib/format';
import StatusBadge from './StatusBadge';

export default function RunsTable({ runs }: { runs: Run[] }) {
  if (runs.length === 0) {
    return <p className="py-6 text-center text-base-content/60">No runs yet.</p>;
  }
  return (
    <div className="overflow-x-auto rounded-box border border-base-300 bg-base-100">
      <table className="table table-zebra table-sm">
        <thead>
          <tr>
            <th>Run</th>
            <th>Definition</th>
            <th>Status</th>
            <th>Trigger</th>
            <th>Started</th>
            <th className="text-right">Duration</th>
          </tr>
        </thead>
        <tbody>
          {runs.map(run => (
            <tr key={run.run_id} className="hover">
              <td>
                <Link href={`/runs/${run.run_id}`} className="link link-hover font-mono text-xs" title={run.run_id}>
                  {shortId(run.run_id)}
                </Link>
              </td>
              <td>
                <Link href={`/jobs/${run.job}`} className="link link-hover">
                  {run.job}
                </Link>
              </td>
              <td>
                <StatusBadge status={run.status} />
              </td>
              <td>{run.trigger}</td>
              <td className="whitespace-nowrap">{formatTimestamp(run.started_at ?? run.queued_at)}</td>
              <td className="whitespace-nowrap text-right font-mono text-xs">
                {isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
