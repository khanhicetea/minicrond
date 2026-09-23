import { useState, type ReactNode } from 'react';
import { Link, useLocation, useParams } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon, type IconName } from '../components/Icon';
import LogViewer from '../components/LogViewer';
import StatusBadge from '../components/StatusBadge';
import { RunsList } from '../components/RunsTable';
import { api, errorText } from '../api';
import { jobQuery, runAlertsQuery, runQuery, runsQuery, useStopRun, useTriggerJob } from '../queries';
import { decodePayload } from '../lib/ansi';
import { downloadFile } from '../lib/download';
import { formatSpan, formatTimestamp, shortId, shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';
import { isActiveRun } from '../types';

type FilterKey = 'all' | 'failed' | 'scheduled' | 'manual';

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'failed', label: 'Failed' },
  { key: 'scheduled', label: 'Scheduled' },
  { key: 'manual', label: 'Manual' },
];

/**
 * Run detail: every fact appears exactly once.
 * - Header: identity (job, status, run ID, trigger) + actions
 * - Essentials row: duration, exit code, started, ended
 * - Collapsible "Run details": attempt, queue/schedule times, process,
 *   definition revision/hash, run-as — hidden until asked for
 * - Output panel: log size in its header; a failed run adds a single-line
 *   hint with "copy last errors" (no repeated status/exit code)
 */
export default function RunDetail() {
  const params = useParams();
  const id = params.id ?? '';
  const [, navigate] = useLocation();
  const run = useQuery(runQuery(id));
  const alerts = useQuery({ ...runAlertsQuery(id), enabled: Boolean(id) && run.isSuccess });
  const stop = useStopRun();
  const trigger = useTriggerJob();
  const [downloadError, setDownloadError] = useState('');
  const [actionError, setActionError] = useState('');
  const [copiedError, setCopiedError] = useState(false);
  const [copiedId, setCopiedId] = useState(false);
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [filter, setFilter] = useState<FilterKey>('all');

  const jobId = run.data?.job ?? '';
  const jobDetail = useQuery({ ...jobQuery(jobId), enabled: Boolean(jobId) });
  const siblings = useQuery(runsQuery(jobId, 30));

  const filteredSiblings = (siblings.data ?? []).filter(sibling => {
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
  const exitCode = data.exit_code;
  const runAs = jobDetail.data?.definition.run_as;
  const hero = heroIcon(data.status);
  const triggerLabel = data.trigger === 'manual' ? 'Manual run' : data.trigger === 'schedule' ? 'Scheduled run' : `${data.trigger} run`;

  const copyId = async () => {
    try {
      await navigator.clipboard.writeText(data.run_id);
      setCopiedId(true);
      setTimeout(() => setCopiedId(false), 2000);
    } catch {
      setCopiedId(false);
    }
  };

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

  const rerun = () => {
    setActionError('');
    trigger.mutate(data.job, {
      onSuccess: next => navigate(`/runs/${next.run_id}`),
      onError: err => setActionError(errorText(err)),
    });
  };

  return (
    <div className="space-y-4">
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link href="/jobs">Jobs &amp; Workers</Link>
        <Icon name="chevron-right" size={12} className="faint" />
        <Link href={jobPath(data.job)}>{data.job}</Link>
        <Icon name="chevron-right" size={12} className="faint" />
        <span>Run</span>
      </nav>

      {/* Header: identity + actions. Status, run ID, and trigger appear only here. */}
      <section className="panel overflow-hidden">
        <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-3 px-5 pb-4 pt-4">
          <div className="flex min-w-0 items-center gap-3.5">
            <span className={`icon-badge h-11 w-11 ${hero.cls}`}>
              <Icon name={hero.icon} size={20} className={hero.spin ? 'spin' : ''} />
            </span>
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <h1 className="truncate font-mono text-lg font-bold tracking-tight">{data.job}</h1>
                <StatusBadge status={data.status} size="md" />
              </div>
              <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs muted">
                <button
                  type="button"
                  className="inline-flex items-center gap-1 font-mono transition-colors hover:text-base-content"
                  title="Copy full run ID"
                  onClick={() => void copyId()}
                >
                  #{shortRunId(data.run_id)}
                  <Icon name={copiedId ? 'check' : 'copy'} size={11} />
                </button>
                <span aria-hidden>·</span>
                <span>{triggerLabel}</span>
              </div>
            </div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {running && (
              <button type="button" className="btn-danger-x" disabled={stop.isPending} onClick={() => stop.mutate(id)}>
                <Icon name="square" size={13} />
                {stop.isPending ? 'Stopping…' : 'Stop'}
              </button>
            )}
            {!running && (
              <button type="button" className="btn-sub" disabled={trigger.isPending} onClick={rerun}>
                <Icon name="play" size={13} />
                {trigger.isPending ? 'Starting…' : 'Run again'}
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

        {/* Essentials: the four facts that answer "how did this run go?" */}
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 border-t border-base-300 px-5 py-4 sm:grid-cols-4">
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
        </div>

        {/* Everything else, collapsed by default. */}
        <div className="border-t border-base-300 bg-base-200/50">
          <button
            type="button"
            className="flex w-full items-center gap-2 px-5 py-2.5 text-xs font-semibold uppercase tracking-wider faint transition-colors hover:text-base-content"
            aria-expanded={detailsOpen}
            onClick={() => setDetailsOpen(open => !open)}
          >
            <Icon name="chevron-down" size={13} className={`transition-transform ${detailsOpen ? '' : '-rotate-90'}`} />
            Run details
            <span className="font-normal normal-case tracking-normal">(attempt, scheduling, process, definition)</span>
          </button>
          {detailsOpen && (
            <div className="grid grid-cols-2 gap-x-6 gap-y-3 px-5 pb-4 sm:grid-cols-3 xl:grid-cols-4">
              <Meta label="Attempt">
                <span className="num">{data.attempt}</span>
              </Meta>
              <Meta label="Queued">{formatTimestamp(data.queued_at)}</Meta>
              <Meta label="Scheduled for">{data.scheduled_for ? formatTimestamp(data.scheduled_for) : '—'}</Meta>
              <Meta label="End reason">{data.end_reason || '—'}</Meta>
              <Meta label="Revision">
                <span className="num">r{data.revision}</span>
              </Meta>
              <Meta label="Definition hash" mono>
                {shortId(data.definition_hash, 16)}
              </Meta>
              <Meta label="Process" mono>
                {data.pid || data.pgid ? `${data.pid || '—'} / ${data.pgid || '—'}` : '—'}
              </Meta>
              <Meta label="Run as">
                {runAs ? <span className="font-mono">{runAs}</span> : <span className="faint">—</span>}
              </Meta>
            </div>
          )}
        </div>
      </section>

      {(downloadError || actionError || stop.error) && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-2.5 text-sm text-red-300">
          {downloadError || actionError || errorText(stop.error)}
        </div>
      )}
      {data.log_truncated && (
        <div role="alert" className="panel flex items-center gap-2 border-amber-500/40 bg-amber-500/5 px-4 py-2.5 text-sm text-amber-300">
          <Icon name="alert-triangle" size={15} />
          Stored log exceeded the size cap and was truncated.
        </div>
      )}

      <section className="panel p-4 sm:p-5" aria-label="Alert deliveries">
        <h2 className="panel-title mb-3">Alert deliveries</h2>
        {alerts.isPending ? (
          <p className="text-sm muted">Loading alert deliveries…</p>
        ) : alerts.isError ? (
          <p role="alert" className="text-sm text-red-300">Failed to load alert deliveries: {errorText(alerts.error)}</p>
        ) : alerts.data.length === 0 ? (
          <p className="text-sm muted">No alert deliveries for this run.</p>
        ) : (
          <ul className="space-y-2">
            {alerts.data.map(item => (
              <li key={item.channel} className="rounded-lg border border-base-300 bg-base-200/25 p-3 text-sm">
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                  <span className="min-w-0 break-all font-mono font-medium">{item.channel}</span>
                  <span className={`chip ${item.status === 'sent' ? 'chip-success' : ['failed', 'dropped', 'interrupted'].includes(item.status) ? 'chip-error' : 'chip-info'}`}>
                    {item.status}
                  </span>
                  <span className="text-xs muted">{item.attempts} {item.attempts === 1 ? 'attempt' : 'attempts'}</span>
                  <span className="text-xs muted">Updated {formatTimestamp(item.updated_at)}</span>
                </div>
                {item.last_error && <p className="mt-2 break-words text-xs text-red-300">Last error: {item.last_error}</p>}
              </li>
            ))}
          </ul>
        )}
      </section>

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
            <span className="num faint">
              {formatLogSize(data.log_bytes)}
              {data.log_truncated ? ' · truncated' : ''}
            </span>
          </div>
          <LogViewer
            runId={id}
            footer={
              failed ? (
                <div className="flex flex-wrap items-center gap-3 border-t border-red-500/30 bg-red-500/5 px-4 py-2.5">
                  <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-red-500/15 text-red-400">
                    <Icon name="alert-circle" size={15} />
                  </span>
                  <p className="min-w-0 flex-1 text-xs muted">The last errors are near the end of the output above.</p>
                  <button type="button" className="btn-sub !border-red-500/40 !text-red-300" onClick={() => void copyError()}>
                    <Icon name={copiedError ? 'check' : 'copy'} size={13} />
                    {copiedError ? 'Copied!' : 'Copy last errors'}
                  </button>
                </div>
              ) : undefined
            }
          />
        </section>
      </div>
    </div>
  );
}

/** Label-over-value cell in the essentials grid. */
function Fact({ label, emphasize = false, children }: { label: string; emphasize?: boolean; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <div className="text-[0.6875rem] font-semibold uppercase tracking-wider faint">{label}</div>
      <div className={`mt-1.5 truncate text-sm ${emphasize ? 'font-semibold' : 'font-medium'}`}>{children}</div>
    </div>
  );
}

/** Smaller label-over-value cell inside the collapsed details section. */
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
