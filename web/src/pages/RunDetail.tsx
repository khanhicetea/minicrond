import { useMemo, useState, type ReactNode } from 'react';
import { Link, useParams } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon, type IconName } from '../components/Icon';
import LogViewer from '../components/LogViewer';
import StatusBadge from '../components/StatusBadge';
import { RunsList } from '../components/RunsTable';
import { api, errorText } from '../api';
import { jobQuery, runQuery, runsQuery, useStopRun } from '../queries';
import { decodePayload } from '../lib/ansi';
import { downloadFile } from '../lib/download';
import { formatSpan, formatTimestamp, shortId } from '../lib/format';
import { jobPath } from '../lib/routes';
import { isActiveRun } from '../types';

type FilterKey = 'all' | 'failed' | 'scheduled' | 'manual';

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'failed', label: 'Failed' },
  { key: 'scheduled', label: 'Scheduled' },
  { key: 'manual', label: 'Manual' },
];

/** Run detail: summary header, recent-runs sidebar, and the live output panel. */
export default function RunDetail() {
  const params = useParams();
  const id = params.id ?? '';
  const run = useQuery(runQuery(id));
  const stop = useStopRun();
  const [downloadError, setDownloadError] = useState('');
  const [copiedError, setCopiedError] = useState(false);
  const [filter, setFilter] = useState<FilterKey>('all');

  const jobId = run.data?.job ?? '';
  const jobDetail = useQuery({ ...jobQuery(jobId), enabled: Boolean(jobId) });
  const siblings = useQuery(runsQuery(jobId, 30));

  const filteredSiblings = useMemo(() => {
    return (siblings.data ?? []).filter(sibling => {
      switch (filter) {
        case 'failed':
          return ['failed', 'timeout', 'interrupted'].includes(sibling.status);
        case 'scheduled':
          return sibling.trigger === 'schedule';
        case 'manual':
          return sibling.trigger === 'manual';
        default:
          return true;
      }
    });
  }, [siblings.data, filter]);

  if (run.isPending) return <div className="skeleton h-64 w-full" />;
  if (run.isError) {
    return (
      <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-3 text-sm text-red-300">
        Failed to load run: {errorText(run.error)}
      </div>
    );
  }

  const data = run.data;
  const running = isActiveRun(data);
  const failed = ['failed', 'timeout', 'interrupted'].includes(data.status);
  const succeeded = data.status === 'succeeded';
  const exitCode = data.exit_code;
  const runAs = jobDetail.data?.definition.run_as;
  const hero = heroIcon(data.status);

  const copyError = async () => {
    try {
      const log = await api.logFrames(id, 0, 5000);
      const stderr = log.items.filter(frame => frame.stream === 2).slice(-20);
      const message = stderr.map(frame => decodePayload(frame.payload)).join('').trim() || `run ${id} ended with status ${data.status}`;
      await navigator.clipboard.writeText(message);
      setCopiedError(true);
      setTimeout(() => setCopiedError(false), 2000);
    } catch {
      setCopiedError(false);
    }
  };

  return (
    <div className="space-y-4">
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link href="/jobs">Jobs &amp; Workers</Link>
        <Icon name="chevron-right" size={12} className="faint" />
        <Link href={jobPath(data.job)}>{data.job}</Link>
        <Icon name="chevron-right" size={12} className="faint" />
        <span>Run #{shortId(data.run_id, 8)}</span>
      </nav>

      {/* Hero header: identity + actions, key facts, technical metadata. */}
      <section className="panel overflow-hidden">
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-3 px-5 pb-4 pt-4">
          <div className="flex min-w-0 items-center gap-3.5">
            <span className={`icon-badge h-11 w-11 ${hero.cls}`}>
              <Icon name={hero.icon} size={20} className={hero.spin ? 'spin' : ''} />
            </span>
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <h1 className="truncate text-lg font-bold tracking-tight">
                  <Link href={jobPath(data.job)} className="font-mono hover:text-sky-200">
                    {data.job}
                  </Link>
                </h1>
                <StatusBadge status={data.status} size="md" />
              </div>
              <p className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs muted">
                <span className="font-mono">#{shortId(data.run_id, 8)}</span>
                <span aria-hidden>·</span>
                <span>{data.trigger === 'manual' ? 'Manual run' : 'Scheduled run'}</span>
                <span aria-hidden>·</span>
                <span className="num">attempt {data.attempt}</span>
              </p>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {running && (
              <button type="button" className="btn-danger-x" disabled={stop.isPending} onClick={() => stop.mutate(id)}>
                <Icon name="square" size={13} />
                {stop.isPending ? 'Stopping…' : 'Stop'}
              </button>
            )}
            <button
              type="button"
              className="btn-sub"
              onClick={() => void navigator.clipboard.writeText(`curl -s -H "Authorization: Bearer $MINICRON_TOKEN" http://127.0.0.1:7423/api/v1/runs/${data.run_id}`)}
            >
              <Icon name="copy" size={14} />
              Copy as curl
            </button>
            <button
              type="button"
              className="btn-sub"
              onClick={() => {
                setDownloadError('');
                downloadFile(api.rawLogUrl(id), `${data.run_id}.log`, api.download).catch(err => setDownloadError(errorText(err)));
              }}
            >
              <Icon name="download" size={14} />
              Download raw
            </button>
          </div>
        </div>

        {/* Key facts */}
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 border-t border-base-300 px-5 py-4 sm:grid-cols-3 xl:grid-cols-7">
          <Fact label="Duration" emphasize>
            <span className="num">
              {running ? formatSpan(data.started_at ?? data.queued_at) : formatSpan(data.started_at, data.ended_at)}
            </span>
          </Fact>
          <Fact label="Exit code" emphasize>
            {exitCode === undefined ? (
              <span className="faint">—</span>
            ) : (
              <span className={`inline-flex items-center gap-1.5 font-medium ${exitCode === 0 ? 'text-green-400' : 'text-red-400'}`}>
                <span className={`inline-flex h-5 w-5 items-center justify-center rounded-full ${exitCode === 0 ? 'bg-green-500/15' : 'bg-red-500/15'}`}>
                  <Icon name={exitCode === 0 ? 'check' : 'x'} size={11} strokeWidth={3} />
                </span>
                <span className="num">
                  {exitCode}
                  {data.signal ? ` (${data.signal})` : ''}
                </span>
              </span>
            )}
          </Fact>
          <Fact label="Started">{formatTimestamp(data.started_at)}</Fact>
          <Fact label="Ended">{formatTimestamp(data.ended_at)}</Fact>
          <Fact label="Trigger">
            <span className={`chip ${data.trigger === 'manual' ? 'chip-info' : 'chip-neutral'}`}>
              <Icon name={data.trigger === 'manual' ? 'user' : 'calendar'} size={12} />
              {data.trigger === 'schedule' ? 'Scheduled' : data.trigger === 'manual' ? 'Manual' : data.trigger}
            </span>
          </Fact>
          <Fact label="Attempt">
            <span className="num">{data.attempt}</span>
          </Fact>
          <Fact label="Run as">
            {runAs ? <span className="chip chip-info font-mono">{runAs}</span> : <span className="faint">—</span>}
          </Fact>
        </div>

        {/* Technical metadata */}
        <div className="grid grid-cols-2 gap-x-6 gap-y-3 border-t border-base-300 bg-base-200/50 px-5 py-3.5 sm:grid-cols-3 xl:grid-cols-6">
          <Meta label="Revision">
            <span className="num">r{data.revision}</span>
          </Meta>
          <Meta label="Definition hash" mono>
            {shortId(data.definition_hash, 16)}
          </Meta>
          <Meta label="Process" mono>
            {data.pid || data.pgid ? `${data.pid || '—'} / ${data.pgid || '—'}` : '—'}
          </Meta>
          <Meta label="Log output" mono>
            {formatLogSize(data.log_bytes)}
            {data.log_truncated ? ' (truncated)' : ''}
          </Meta>
          <Meta label="Scheduled for">{data.scheduled_for ? formatTimestamp(data.scheduled_for) : '—'}</Meta>
          <Meta label="End reason">{data.end_reason || '—'}</Meta>
        </div>
      </section>

      {(downloadError || stop.error) && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-2.5 text-sm text-red-300">
          {downloadError || errorText(stop.error)}
        </div>
      )}
      {data.log_truncated && (
        <div role="alert" className="panel flex items-center gap-2 border-amber-500/40 bg-amber-500/5 px-4 py-2.5 text-sm text-amber-300">
          <Icon name="alert-triangle" size={15} />
          Stored log exceeded the size cap and was truncated.
        </div>
      )}

      {/* Sidebar + output */}
      <div className="grid gap-4 lg:grid-cols-[20rem_minmax(0,1fr)]">
        <aside className="panel h-fit p-3.5">
          <div className="mb-2.5 flex items-center justify-between">
            <h2 className="panel-title">Recent runs</h2>
            <span className="num text-xs faint">{filteredSiblings.length}</span>
          </div>
          <div className="tab-seg mb-3 w-full">
            {FILTERS.map(f => (
              <button
                key={f.key}
                type="button"
                className={`flex-1 ${filter === f.key ? 'active' : ''}`}
                onClick={() => setFilter(f.key)}
              >
                {f.label}
              </button>
            ))}
          </div>
          <RunsList runs={filteredSiblings} currentId={id} />
          <Link
            href="/runs"
            className="mt-3 flex items-center justify-between border-t border-base-300 px-1 pt-2.5 text-[0.8125rem] text-sky-300 hover:text-sky-200"
          >
            View all runs
            <Icon name="chevron-right" size={14} />
          </Link>
        </aside>

        <section className="panel flex min-h-[32rem] flex-col overflow-hidden">
          <div className="flex items-center justify-between border-b border-base-300 px-4 py-2.5">
            <h2 className="panel-title">Run output</h2>
            <span className="num faint">{data.run_id}</span>
          </div>
          <LogViewer
            runId={id}
            footer={
              failed || succeeded ? (
                <div
                  className={`flex flex-wrap items-center gap-3 border-t px-4 py-3 ${
                    failed ? 'border-red-500/30 bg-red-500/5' : 'border-green-500/30 bg-green-500/5'
                  }`}
                >
                  <span
                    className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-full ${
                      failed ? 'bg-red-500/15 text-red-400' : 'bg-green-500/15 text-green-400'
                    }`}
                  >
                    <Icon name={failed ? 'alert-circle' : 'circle-check'} size={18} />
                  </span>
                  <div className="min-w-0 flex-1">
                    <p className={`text-sm font-semibold ${failed ? 'text-red-400' : 'text-green-400'}`}>
                      {failed ? 'Run failed' : 'Run succeeded'}
                    </p>
                    <p className="num truncate text-xs muted">
                      {failed
                        ? `Exit code ${exitCode ?? '—'} · ${data.end_reason || 'ended'} · last error near the end of output`
                        : `Exit code ${exitCode ?? 0} · ${formatSpan(data.started_at, data.ended_at)}`}
                    </p>
                  </div>
                  {failed && (
                    <button type="button" className="btn-sub !border-red-500/40 !text-red-300" onClick={() => void copyError()}>
                      <Icon name={copiedError ? 'check' : 'copy'} size={13} />
                      {copiedError ? 'Copied!' : 'Copy error'}
                    </button>
                  )}
                </div>
              ) : undefined
            }
          />
        </section>
      </div>
    </div>
  );
}

/** Label-over-value cell in the key-facts grid. */
function Fact({ label, emphasize = false, children }: { label: string; emphasize?: boolean; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-[0.6875rem] font-semibold uppercase tracking-wider faint">{label}</div>
      <div className={`mt-1.5 truncate text-sm ${emphasize ? 'font-semibold' : 'font-medium'}`}>{children}</div>
    </div>
  );
}

/** Smaller label-over-value cell for technical metadata. */
function Meta({ label, mono = false, children }: { label: string; mono?: boolean; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-[0.6875rem] font-semibold uppercase tracking-wider faint">{label}</div>
      <div className={`mt-0.5 truncate text-xs muted ${mono ? 'font-mono' : ''}`}>{children}</div>
    </div>
  );
}

/** Tinted square + icon representing the run status in the header. */
function heroIcon(status: string): { icon: IconName; cls: string; spin?: boolean } {
  if (status === 'succeeded') return { icon: 'circle-check', cls: 'icon-green' };
  if (status === 'failed' || status === 'timeout' || status === 'interrupted')
    return { icon: 'alert-circle', cls: 'icon-red' };
  if (status === 'running' || status === 'pending') return { icon: 'loader', cls: 'icon-blue', spin: true };
  return { icon: 'clock', cls: 'icon-amber' };
}

function formatLogSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
