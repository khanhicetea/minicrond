import { useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import LogViewer from '../components/LogViewer';
import { RunStatusDot } from '../components/RunsTable';
import { Badge, Button, Dot, EmptyState, IconButton, RunStrip, SearchField, Segmented, buttonClass, isFailedStatus, runStatusMeta, type Tone } from '../components/ui';
import { api, errorText } from '../api';
import { downloadFile } from '../lib/download';
import { formatBytes, formatDayTime, formatSpan, formatTimestamp, shortRunId } from '../lib/format';
import { humanizeSchedule } from '../lib/cron';
import { jobPath } from '../lib/routes';
import { memoryMiB } from '../lib/resources';
import { useOverview, workerNeedsAttention } from '../lib/attention';
import { isEmbedded } from '../lib/embed';
import { runQuery, useStopRun, useTriggerJob } from '../queries';
import { isActiveRun, type Definition, type Run } from '../types';

type FilterKey = 'all' | 'active' | 'failed' | 'scheduled' | 'manual';
const FILTERS: { value: FilterKey; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'active', label: 'Active' },
  { value: 'failed', label: 'Failed' },
  { value: 'scheduled', label: 'Scheduled' },
  { value: 'manual', label: 'Manual' },
];

function matchesFilter(run: Run, filter: FilterKey): boolean {
  if (filter === 'active') return isActiveRun(run);
  if (filter === 'failed') return isFailedStatus(run.status);
  if (filter === 'scheduled') return run.trigger === 'schedule';
  if (filter === 'manual') return run.trigger === 'manual';
  return true;
}

function durationMs(run: Run, now: number): number {
  const start = Date.parse(run.started_at ?? run.queued_at);
  const end = run.ended_at ? Date.parse(run.ended_at) : now;
  return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, end - start) : 0;
}

function runSpan(run: Run): string {
  return isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at);
}

const TONE_VAR: Record<Tone, string> = {
  ok: 'var(--mc-ok)',
  danger: 'var(--mc-danger)',
  warn: 'var(--mc-warn)',
  info: 'var(--mc-info)',
  neutral: 'var(--mc-neutral)',
};

/** Jobs → run history → selected run output, as three independent panes. */
export default function Runs() {
  const search = useSearch();
  const [, navigate] = useLocation();
  const params = new URLSearchParams(search);
  const requestedJob = params.get('job');
  const requestedRun = params.get('run');
  const filterParam = params.get('filter');
  const filter: FilterKey = FILTERS.some(item => item.value === filterParam) ? (filterParam as FilterKey) : 'all';
  const [jobSearch, setJobSearch] = useState('');
  const [actionError, setActionError] = useState('');

  const { jobs, definitions: unsorted, runsByJob, workerStates, attention } = useOverview();
  const definitions = [...unsorted].sort((a, b) => a.name.localeCompare(b.name));
  const selectedJob = definitions.find(job => job.name === requestedJob)?.name ?? (requestedJob ? '' : (attention[0]?.definition.name ?? definitions[0]?.name ?? ''));
  const selectedDefinition = definitions.find(job => job.name === selectedJob);
  const configOwned = selectedDefinition?.source === 'config';

  const history = useInfiniteQuery({
    queryKey: ['run-pages', selectedJob, filter],
    queryFn: ({ pageParam, signal }) => api.listRunsPage(selectedJob, 100, pageParam, filter === 'all' ? '' : filter, signal),
    initialPageParam: '',
    getNextPageParam: page => page.next_before || undefined,
    enabled: Boolean(selectedJob),
    // Poll quickly only while a listed run is active; a slower refresh still
    // picks up scheduled and externally triggered runs.
    refetchInterval: query => (query.state.data?.pages.some(page => page.items?.some(isActiveRun)) ? 2_000 : 15_000),
    refetchIntervalInBackground: false,
  });
  const listed = history.data?.pages.flatMap(page => page.items ?? []) ?? [];
  const runId = requestedRun || listed[0]?.run_id || '';
  const detail = useQuery({ ...runQuery(runId), enabled: Boolean(selectedJob && runId) });
  // A freshly triggered run may not have appeared in the history response yet.
  const selectedRun = listed.find(run => run.run_id === runId) ?? (detail.data?.job === selectedJob && matchesFilter(detail.data, filter) ? detail.data : undefined);
  const trigger = useTriggerJob();
  const stop = useStopRun();

  const go = (job: string, run = '', nextFilter: FilterKey = filter) => {
    setActionError('');
    const next = new URLSearchParams();
    if (job) next.set('job', job);
    if (run) next.set('run', run);
    if (nextFilter !== 'all') next.set('filter', nextFilter);
    void navigate(`/runs${next.size ? `?${next}` : ''}`);
  };

  const rerun = () => {
    if (!selectedJob) return;
    setActionError('');
    trigger.mutate(selectedJob, {
      onSuccess: next => go(selectedJob, next.run_id, 'all'),
      onError: err => setActionError(errorText(err)),
    });
  };

  const jobTone = (job: Definition): Tone => {
    if (job.enabled === false) return 'neutral';
    if (job.kind === 'worker') {
      const state = workerStates[job.name];
      return workerNeedsAttention(state) ? 'warn' : state?.active ? 'ok' : 'neutral';
    }
    const last = runsByJob.get(job.name)?.[0];
    return last ? runStatusMeta(last.status).tone : 'neutral';
  };

  const q = jobSearch.trim().toLowerCase();
  const attentionNames = new Set(attention.map(item => item.definition.name));
  const matching = definitions.filter(job => job.name.toLowerCase().includes(q));
  const groups = [
    { label: 'Needs attention', attention: true, jobs: matching.filter(job => attentionNames.has(job.name)) },
    { label: attentionNames.size ? 'All' : '', attention: false, jobs: matching.filter(job => !attentionNames.has(job.name)) },
  ].filter(group => group.jobs.length > 0);

  const now = Date.now();
  const longest = Math.max(1, ...listed.map(run => durationMs(run, now)));
  const stripRuns = filter === 'all' && listed.length > 0 ? listed : (runsByJob.get(selectedJob) ?? []);

  return (
    <div className="runs-workspace" aria-label="Run history">
      {/* Pane 1: definitions */}
      <aside className="runs-pane runs-pane-jobs" aria-label="Jobs and workers">
        <div className="runs-pane-header">
          <div className="flex items-center justify-between gap-2">
            <h1 className="panel-title">Jobs &amp; workers</h1>
            <span className="text-xs faint tabular">{definitions.length}</span>
          </div>
          <SearchField size="sm" value={jobSearch} onChange={setJobSearch} label="Find a job" />
        </div>
        <div className="runs-pane-scroll">
          {jobs.isPending && <p className="p-3 text-sm muted">Loading jobs…</p>}
          {jobs.isError && (
            <p role="alert" className="p-3 text-sm text-danger-fg">
              {errorText(jobs.error)}
            </p>
          )}
          {!jobs.isPending && definitions.length === 0 && <p className="p-3 text-sm muted">No jobs or workers yet.</p>}
          {definitions.length > 0 && matching.length === 0 && <p className="p-3 text-sm muted">No matching jobs.</p>}
          {groups.map(group => (
            <div key={group.label || 'all'}>
              {group.label && <div className={`pane-group-label ${group.attention ? 'attention' : ''}`}>{group.label}</div>}
              {group.jobs.map(job => {
                const last = runsByJob.get(job.name)?.[0];
                return (
                  <button key={job.name} type="button" className="runs-job-row" onClick={() => go(job.name)} aria-pressed={selectedJob === job.name}>
                    <Dot tone={jobTone(job)} />
                    <span className="min-w-0 flex-1 truncate mono text-[0.78rem]" title={job.name}>
                      {job.name}
                    </span>
                    <span className="shrink-0 text-[0.72rem] faint">{job.kind === 'worker' ? 'worker' : last ? formatDayTime(last.started_at ?? last.queued_at) : ''}</span>
                  </button>
                );
              })}
            </div>
          ))}
        </div>
      </aside>

      {/* Pane 2: runs of the selected definition */}
      <section className="runs-pane runs-pane-runs" aria-label="Runs for selected job">
        <div className="runs-pane-header">
          <div className="flex min-w-0 items-start gap-2">
            <div className="min-w-0 flex-1">
              <h2 className="truncate mono text-sm font-semibold" title={selectedJob}>
                {selectedJob || 'Select a job'}
              </h2>
              {selectedDefinition && (
                <p className="truncate text-xs muted">
                  {selectedDefinition.kind === 'worker' ? 'Worker · continuous' : `${humanizeSchedule(selectedDefinition.schedule ?? '') || selectedDefinition.schedule || 'Manual only'}`}
                </p>
              )}
            </div>
            {selectedJob && (
              <Link href={jobPath(selectedJob)} className="shrink-0 text-xs leading-7 text-link hover:text-link-hover">
                {configOwned ? 'View' : 'Edit'}
              </Link>
            )}
          </div>
          {selectedJob && <RunStrip runs={stripRuns} size="lg" />}
          <Segmented<FilterKey> size="sm" label="Filter runs" value={filter} onChange={value => go(selectedJob, '', value)} options={FILTERS} />
        </div>
        <div className="runs-pane-scroll">
          {!selectedJob && <p className="p-3 text-sm muted">Select a job to see its runs.</p>}
          {selectedJob && history.isPending && <p className="p-3 text-sm muted">Loading runs…</p>}
          {history.isError && (
            <p role="alert" className="p-3 text-sm text-danger-fg">
              {errorText(history.error)}
            </p>
          )}
          {history.isSuccess && listed.length === 0 && <p className="p-3 text-sm muted">No runs match this filter.</p>}
          <ol className="m-0 list-none p-0">
            {listed.map(run => {
              const meta = runStatusMeta(run.status);
              const width = Math.max(3, Math.round((100 * durationMs(run, now)) / longest));
              return (
                <li key={run.run_id}>
                  <button type="button" className="runs-history-row" onClick={() => go(selectedJob, run.run_id)} aria-pressed={selectedRun?.run_id === run.run_id}>
                    <RunStatusDot status={run.status} />
                    <span className="flex min-w-0 flex-1 flex-col gap-1">
                      <span className="flex items-center justify-between gap-2">
                        <span className="truncate mono text-[0.78rem] font-medium">{formatDayTime(run.started_at ?? run.queued_at)}</span>
                        <span className="shrink-0 text-xs text-fg-muted tabular">{runSpan(run)}</span>
                      </span>
                      <span className="duration-track" aria-hidden="true">
                        <span style={{ width: `${width}%`, background: TONE_VAR[meta.tone] }} />
                      </span>
                      <span className="flex items-center justify-between gap-2 text-[0.72rem] faint">
                        <span>
                          {meta.label} · {run.trigger}
                        </span>
                        <span className="mono">#{shortRunId(run.run_id)}</span>
                      </span>
                    </span>
                  </button>
                </li>
              );
            })}
          </ol>
          {history.hasNextPage && (
            <div className="p-2">
              <Button size="sm" className="w-full" busy={history.isFetchingNextPage} onClick={() => void history.fetchNextPage()}>
                Load older runs
              </Button>
            </div>
          )}
        </div>
      </section>

      {/* Pane 3: selected run output */}
      <section className="runs-pane runs-pane-output" aria-label="Selected run output">
        {selectedJob && runId && detail.isPending ? (
          <div className="p-5 text-sm muted">Loading run output…</div>
        ) : selectedJob && runId && detail.isError ? (
          <div role="alert" className="p-5 text-sm text-danger-fg">
            Failed to load run: {errorText(detail.error)}
          </div>
        ) : !selectedRun || !detail.isSuccess ? (
          <div className="flex flex-1 items-center justify-center">
            <EmptyState icon="terminal" title="Select a run to view its output." />
          </div>
        ) : (
          <RunOutput
            run={detail.data}
            configOwned={configOwned}
            rerunBusy={trigger.isPending}
            stopBusy={stop.isPending}
            actionError={actionError}
            onRerun={rerun}
            onStop={() => {
              setActionError('');
              stop.mutate(detail.data.run_id, { onError: err => setActionError(errorText(err)) });
            }}
            onDownloadError={message => setActionError(message)}
          />
        )}
      </section>
    </div>
  );
}

function RunOutput({
  run,
  configOwned,
  rerunBusy,
  stopBusy,
  actionError,
  onRerun,
  onStop,
  onDownloadError,
}: {
  run: Run;
  configOwned: boolean;
  rerunBusy: boolean;
  stopBusy: boolean;
  actionError: string;
  onRerun: () => void;
  onStop: () => void;
  onDownloadError: (message: string) => void;
}) {
  const meta = runStatusMeta(run.status);
  const active = isActiveRun(run);
  const failed = isFailedStatus(run.status);
  const headline = active
    ? `Running for ${runSpan(run)}`
    : run.exit_code !== undefined && !(failed && run.exit_code === 0)
      ? `Exit ${run.exit_code}${run.signal ? ` (${run.signal})` : ''} ${run.exit_code === 0 ? 'in' : 'after'} ${runSpan(run)}`
      : `${meta.label} after ${runSpan(run)}`;
  const usage = run.resource_usage;
  return (
    <>
      <div className="runs-output-header">
        <div className="min-w-0 flex-[1_1_14rem]">
          <div className="flex flex-wrap items-center gap-2">
            <Badge tone={meta.tone} icon={meta.icon} spin={meta.spin}>
              {meta.label}
            </Badge>
            <span className="text-[0.95rem] font-semibold tabular">{headline}</span>
          </div>
          <p className="mt-0.5 truncate text-xs muted" title={run.run_id}>
            {formatTimestamp(run.started_at ?? run.queued_at)} · {run.trigger} · <span className="mono">#{shortRunId(run.run_id)}</span>
            {run.attempt > 1 && ` · attempt ${run.attempt}`}
          </p>
        </div>
        {usage && !isEmbedded && (
          <span className="usage-chip" title="User / system CPU and peak RSS of the direct process; CPU may include waited-for descendants.">
            CPU {(usage.user_cpu_us / 1e6).toFixed(2)}s / {(usage.system_cpu_us / 1e6).toFixed(2)}s<span className="faint">|</span>RSS {memoryMiB(usage.peak_rss_bytes)}
          </span>
        )}
        <div className="flex flex-wrap items-center gap-1.5">
          {active ? (
            <Button size="sm" variant="danger" icon="square" busy={stopBusy} onClick={onStop}>
              Stop
            </Button>
          ) : !configOwned ? (
            <Button size="sm" variant="primary" icon="play" busy={rerunBusy} onClick={onRerun}>
              Run again
            </Button>
          ) : null}
          <Link href={`/runs/${encodeURIComponent(run.run_id)}`} className={buttonClass('secondary', 'sm')}>
            Details
          </Link>
          <IconButton
            icon="download"
            label="Download raw log"
            size="sm"
            outlined
            onClick={() => {
              downloadFile(api.rawLogUrl(run.run_id), 'minicron-run.log', api.download).catch(err => onDownloadError(errorText(err)));
            }}
          />
        </div>
      </div>
      {actionError && (
        <p role="alert" className="border-b border-line px-4 py-2 text-xs text-danger-fg">
          {actionError}
        </p>
      )}
      {run.log_truncated && (
        <p role="alert" className="border-b border-line px-4 py-2 text-xs text-warn-fg">
          Stored log exceeded the size cap and was truncated.
        </p>
      )}
      <div className="flex min-h-0 flex-1 flex-col">
        <LogViewer key={run.run_id} runId={run.run_id} errorTail={failed} />
      </div>
      <div className="flex flex-wrap items-center gap-3 border-t border-line-subtle px-4 py-2 text-xs faint">
        <span>{active ? 'Run in progress · new output appears within a few seconds' : 'End of output'}</span>
        <span className="tabular">{formatBytes(run.log_bytes)}</span>
      </div>
    </>
  );
}
