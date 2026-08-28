import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { Link, useParams } from 'wouter';
import { api, errorText } from '../api';
import LogViewer from '../components/LogViewer';
import StatusBadge from '../components/StatusBadge';
import { downloadFile } from '../lib/download';
import { formatBytes, formatSpan, formatTimestamp, shortId } from '../lib/format';
import { runQuery, useStopRun } from '../queries';

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs font-semibold uppercase tracking-wide text-base-content/50">{label}</dt>
      <dd className="mt-0.5 break-words font-mono text-sm">{children}</dd>
    </div>
  );
}

export default function RunDetail() {
  const params = useParams();
  const id = params.id ?? '';
  const run = useQuery(runQuery(id));
  const stop = useStopRun();
  const [downloadError, setDownloadError] = useState('');

  if (run.isPending) return <div className="skeleton h-64 w-full" />;
  if (run.isError) {
    return (
      <div role="alert" className="alert alert-error">
        <span>Failed to load run: {errorText(run.error)}</span>
      </div>
    );
  }

  const data = run.data;
  const running = data.status === 'running' || data.status === 'pending';

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-2xl font-bold">
          Run <span className="font-mono text-xl">{shortId(data.run_id)}</span>
        </h1>
        <StatusBadge status={data.status} />
        <Link href={`/jobs/${data.job}`} className="link link-hover font-semibold">
          {data.job}
        </Link>
        <div className="ml-auto flex gap-2">
          <button
            type="button"
            className="btn btn-warning btn-outline btn-sm"
            disabled={!running || stop.isPending}
            onClick={() => stop.mutate(id)}
          >
            {stop.isPending && <span className="loading loading-spinner loading-xs" />}
            Stop
          </button>
          <button
            type="button"
            className="btn btn-outline btn-sm"
            onClick={() => {
              setDownloadError('');
              downloadFile(api.rawLogUrl(id), `${id}.log`, api.download).catch(err => setDownloadError(errorText(err)));
            }}
          >
            Download raw log
          </button>
        </div>
      </div>
      {stop.error && (
        <div role="alert" className="alert alert-error text-sm">
          <span>{errorText(stop.error)}</span>
        </div>
      )}
      {downloadError && (
        <div role="alert" className="alert alert-error text-sm">
          <span>{downloadError}</span>
        </div>
      )}
      {data.log_truncated && (
        <div role="alert" className="alert alert-warning text-sm">
          <span>Stored log exceeded the size cap and was truncated.</span>
        </div>
      )}

      <div className="card border border-base-300 bg-base-100">
        <div className="card-body grid grid-cols-2 gap-4 py-4 sm:grid-cols-3 lg:grid-cols-4">
          <Detail label="Status">
            <StatusBadge status={data.status} />
          </Detail>
          <Detail label="Trigger">{data.trigger}</Detail>
          <Detail label="Attempt">{data.attempt}</Detail>
          <Detail label="Kind">{data.kind}</Detail>
          <Detail label="Queued">{formatTimestamp(data.queued_at)}</Detail>
          <Detail label="Started">{formatTimestamp(data.started_at)}</Detail>
          <Detail label="Ended">{formatTimestamp(data.ended_at)}</Detail>
          <Detail label="Duration">
            {formatSpan(data.started_at ?? data.queued_at, data.ended_at)}
          </Detail>
          <Detail label="Scheduled for">{data.scheduled_for ? formatTimestamp(data.scheduled_for) : '—'}</Detail>
          <Detail label="Exit">{data.exit_code ?? '—'}{data.signal ? ` (${data.signal})` : ''}</Detail>
          <Detail label="End reason">{data.end_reason || '—'}</Detail>
          <Detail label="PID / PGID">{data.pid || data.pgid ? `${data.pid || '—'} / ${data.pgid || '—'}` : '—'}</Detail>
          <Detail label="Definition revision">r{data.revision}</Detail>
          <Detail label="Definition hash">{shortId(data.definition_hash)}</Detail>
          <Detail label="Log size">
            {formatBytes(data.log_bytes)}
            {data.log_truncated ? ' (truncated)' : ''}
          </Detail>
          {data.missed_count ? <Detail label="Missed during downtime">{data.missed_count}</Detail> : null}
        </div>
      </div>

      <section className="space-y-2">
        <h2 className="text-lg font-semibold">Log</h2>
        <LogViewer runId={id} />
      </section>
    </div>
  );
}
