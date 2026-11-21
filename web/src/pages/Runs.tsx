import { useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import LogViewer from '../components/LogViewer';
import { Icon } from '../components/Icon';
import { Badge, Button, Dot, EmptyState, IconButton, SearchField, buttonClass, isFailedStatus, runStatusMeta, type Tone } from '../components/ui';
import { api, errorText } from '../api';
import { downloadFile } from '../lib/download';
import { formatBytes, formatDayTime, formatSpan, formatTimestamp, shortRunId } from '../lib/format';
import { humanizeSchedule } from '../lib/cron';
import { jobPath } from '../lib/routes';
import { memoryMiB } from '../lib/resources';
import { useOverview } from '../lib/attention';
import { runQuery, useStopRun, useTriggerJob } from '../queries';
import { isActiveRun, type Definition, type Run, type WorkerState } from '../types';

/** Runs shown when a job is first expanded, and how many each "Load more" adds. */
const FIRST_PAGE = 5;
const MORE_PAGE = 20;
/** Bars in each job's run graph. */
const GRAPH_SLOTS = 30;

const TONE_VAR: Record<Tone, string> = {
  ok: 'var(--mc-ok)',
  danger: 'var(--mc-danger)',
  warn: 'var(--mc-warn)',
  info: 'var(--mc-info)',
  neutral: 'var(--mc-neutral)',
};

function durationMs(run: Run, now: number): number {
  const start = Date.parse(run.started_at ?? run.queued_at);
  const end = run.ended_at ? Date.parse(run.ended_at) : now;
  return Number.isFinite(start) && Number.isFinite(end) ? Math.max(0, end - start) : 0;
}

function runSpan(run: Run): string {
  return isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at);
}

const WORKER_STATUS: Record<WorkerState['status'], { label: string; tone: Tone }> = {
  running: { label: 'Running', tone: 'ok' },
  restarting: { label: 'Restarting', tone: 'warn' },
  held: { label: 'Held', tone: 'warn' },
  exhausted: { label: 'Exhausted', tone: 'danger' },
  stopped: { label: 'Stopped', tone: 'neutral' },
};

/** The status shown at the right of a job row: the worker state, or the latest run. */
function jobStatus(job: Definition, recent: Run[], worker: WorkerState | undefined): { label: string; tone: Tone } {
  if (job.enabled === false) return { label: 'Disabled', tone: 'neutral' };
  if (job.kind === 'worker') {
    if (!worker) return { label: 'Worker', tone: 'neutral' };
    const meta = WORKER_STATUS[worker.status] ?? { label: worker.status, tone: 'neutral' };
    return worker.failures > 0 && meta.tone === 'ok' ? { label: `${worker.failures} failures`, tone: 'warn' } : meta;
  }
  const last = recent[0];
  if (!last) return { label: 'No runs', tone: 'neutral' };
  const meta = runStatusMeta(last.status);
  if (!isFailedStatus(last.status)) return { label: meta.label, tone: meta.tone };
  let streak = 0;
  while (streak < recent.length && isFailedStatus(recent[streak].status)) streak++;
  return { label: streak > 1 ? `${meta.label} ${streak}×` : meta.label, tone: meta.tone };
}

/**
 * One bar per recent run, oldest on the left: height is the run's duration
 * (square-root scale, so one long run does not flatten the rest), color its status.
 */
function RunGraph({ runs, now }: { runs: Run[]; now: number }) {
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

/** Two panes: jobs and workers (the selected one expands into its runs) → selected run detail. */
export default function Runs() {
  const search = useSearch();
  const [, navigate] = useLocation();
  const params = new URLSearchParams(search);
  const requestedJob = params.get('job');
  const requestedRun = params.get('run');
  const [jobSearch, setJobSearch] = useState('');
  const [collapsed, setCollapsed] = useState(false);
  const [actionError, setActionError] = useState('');

  const { jobs, definitions: unsorted, runsByJob, workerStates, attention } = useOverview();
  const definitions = [...unsorted].sort((a, b) => a.name.localeCompare(b.name));
  const selectedJob = definitions.find(job => job.name === requestedJob)?.name ?? (requestedJob ? '' : (attention[0]?.definition.name ?? definitions[0]?.name ?? ''));
  const selectedDefinition = definitions.find(job => job.name === selectedJob);
  const configOwned = selectedDefinition?.source === 'config';

  const history = useInfiniteQuery({
    queryKey: ['run-tree', selectedJob],
    queryFn: ({ pageParam, signal }) => api.listRunsPage(selectedJob, pageParam.limit, pageParam.before, '', signal),
    initialPageParam: { before: '', limit: FIRST_PAGE },
    getNextPageParam: page => (page.next_before ? { before: page.next_before, limit: MORE_PAGE } : undefined),
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
  const selectedRun = listed.find(run => run.run_id === runId) ?? (detail.data?.job === selectedJob ? detail.data : undefined);
  const trigger = useTriggerJob();
  const stop = useStopRun();

  const go = (job: string, run = '') => {
    setActionError('');
    const next = new URLSearchParams();
    if (job) next.set('job', job);
    if (run) next.set('run', run);
    void navigate(`/runs${next.size ? `?${next}` : ''}`);
  };

  const pickJob = (job: string) => {
    if (job === selectedJob) {
      setCollapsed(value => !value);
      return;
    }
    setCollapsed(false);
    go(job);
  };

  const rerun = () => {
    if (!selectedJob) return;
    setActionError('');
    trigger.mutate(selectedJob, {
      onSuccess: next => go(selectedJob, next.run_id),
      onError: err => setActionError(errorText(err)),
    });
  };

  const q = jobSearch.trim().toLowerCase();
  const attentionNames = new Set(attention.map(item => item.definition.name));
  const matching = definitions.filter(job => job.name.toLowerCase().includes(q));
  const groups = [
    { label: 'Needs attention', attention: true, jobs: matching.filter(job => attentionNames.has(job.name)) },
    { label: 'All', attention: false, jobs: matching.filter(job => !attentionNames.has(job.name)) },
  ].filter(group => group.jobs.length > 0);

  const now = Date.now();

  return (
    <div className="runs-workspace" aria-label="Run history">
      {/* Pane 1: definitions; the selected one expands into its runs */}
      <aside className="runs-pane runs-pane-jobs" aria-label="Jobs and workers">
        <div className="runs-pane-header">
          <SearchField size="sm" value={jobSearch} onChange={setJobSearch} label="Find a job or worker" />
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
            <div key={group.label}>
              <div className={`pane-group-label ${group.attention ? 'attention' : ''}`}>
                {group.label} ({group.jobs.length})
              </div>
              {group.jobs.map(job => {
                const recent = runsByJob.get(job.name) ?? [];
                const status = jobStatus(job, recent, workerStates[job.name]);
                const last = recent[0];
                const selected = job.name === selectedJob;
                const expanded = selected && !collapsed;
                const schedule = job.kind === 'worker' ? 'Worker' : humanizeSchedule(job.schedule ?? '') || job.schedule || 'Manual only';
                return (
                  <div key={job.name} className="runs-job" data-selected={selected || undefined}>
                    <button type="button" className="runs-job-head" onClick={() => pickJob(job.name)} aria-expanded={expanded}>
                      <span className="runs-job-title">
                        <Icon name="chevron-right" size={12} className={`runs-job-chevron ${expanded ? 'open' : ''}`} />
                        <span className="min-w-0 flex-1 truncate mono text-[0.8rem] font-medium" title={job.name}>
                          {job.name}
                        </span>
                        <span className={`runs-job-status tone-${status.tone}`}>
                          {status.label}
                          <Dot tone={status.tone} />
                        </span>
                      </span>
                      <span className="runs-job-graph">
                        <RunGraph runs={recent} now={now} />
                        <span className="runs-job-meta">
                          <span className="truncate" title={schedule}>
                            {schedule}
                          </span>
                          <span className="truncate">{last ? formatDayTime(last.started_at ?? last.queued_at) : ''}</span>
                        </span>
                      </span>
                    </button>
                    {expanded && (
                      <div className="runs-tree-wrap">
                        {history.isPending && <p className="px-3 py-2 text-xs muted">Loading runs…</p>}
                        {history.isError && (
                          <p role="alert" className="px-3 py-2 text-xs text-danger-fg">
                            {errorText(history.error)}
                          </p>
                        )}
                        {history.isSuccess && listed.length === 0 && <p className="px-3 py-2 text-xs muted">No runs yet.</p>}
                        {listed.length > 0 && (
                          <ol className="runs-tree" aria-label={`Runs of ${job.name}`}>
                            {listed.map(run => {
                              const meta = runStatusMeta(run.status);
                              const current = selectedRun?.run_id === run.run_id;
                              return (
                                <li key={run.run_id}>
                                  <button
                                    type="button"
                                    className="runs-tree-row"
                                    onClick={() => go(job.name, run.run_id)}
                                    aria-pressed={current}
                                    title={`${meta.label} · #${shortRunId(run.run_id)}`}
                                  >
                                    <span className="runs-tree-tick" aria-hidden="true" />
                                    <Dot tone={meta.tone} pulse={isActiveRun(run)} />
                                    <span className="min-w-0 truncate">
                                      <span className="mono text-[0.75rem]">{formatDayTime(run.started_at ?? run.queued_at)}</span>
                                      {run.trigger !== 'schedule' && <span className="ml-1.5 text-[0.72rem] faint">{run.trigger}</span>}
                                    </span>
                                    <span className="mono text-right text-[0.72rem] text-fg-muted tabular">{runSpan(run)}</span>
                                  </button>
                                </li>
                              );
                            })}
                          </ol>
                        )}
                        <div className="runs-tree-footer">
                          {history.hasNextPage && (
                            <Button size="sm" busy={history.isFetchingNextPage} onClick={() => void history.fetchNextPage()}>
                              Load {MORE_PAGE} more
                            </Button>
                          )}
                          {listed.length > 0 && <span className="text-xs faint">{listed.length} shown</span>}
                          <Link href={jobPath(job.name)} className="ml-auto text-xs text-link hover:text-link-hover">
                            {job.source === 'config' ? 'View' : 'Edit'} {job.kind === 'worker' ? 'worker' : 'job'}
                          </Link>
                        </div>
                      </div>
                    )}
                  </div>
                );
              })}
            </div>
          ))}
        </div>
      </aside>

      {/* Pane 2: selected run detail */}
      <section className="runs-pane runs-pane-output" aria-label="Selected run output">
        {selectedJob && runId && detail.isPending ? (
          <div className="p-5 text-sm muted">Loading run output…</div>
        ) : selectedJob && runId && detail.isError ? (
          <div role="alert" className="p-5 text-sm text-danger-fg">
            Failed to load run: {errorText(detail.error)}
          </div>
        ) : !selectedRun || !detail.isSuccess ? (
          <div className="flex flex-1 items-center justify-center">
            <EmptyState icon="terminal" title={selectedJob && history.isSuccess && listed.length === 0 ? 'This job has no runs yet.' : 'Select a run to view its output.'} />
          </div>
        ) : (
          <RunOutput
            run={detail.data}
            latest={detail.data.run_id === listed[0]?.run_id}
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
  latest,
  configOwned,
  rerunBusy,
  stopBusy,
  actionError,
  onRerun,
  onStop,
  onDownloadError,
}: {
  run: Run;
  latest: boolean;
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
        <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-1.5 text-xs faint">
          <Link href={jobPath(run.job)} className="truncate mono text-link hover:text-link-hover">
            {run.job}
          </Link>
          <span aria-hidden="true">/</span>
          <span className="mono text-fg-muted" title={run.run_id}>
            #{shortRunId(run.run_id)}
          </span>
          {latest && <span className="runs-latest-tag">latest</span>}
        </nav>
        <div className="flex flex-wrap items-center gap-2.5">
          <Badge tone={meta.tone} icon={meta.icon} spin={meta.spin}>
            {meta.label}
          </Badge>
          <span className="text-base font-semibold tabular">{headline}</span>
          <div className="ml-auto flex flex-wrap items-center gap-1.5">
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
                downloadFile(api.rawLogUrl(run.run_id), 'minicrond-run.log', api.download).catch(err => onDownloadError(errorText(err)));
              }}
            />
          </div>
        </div>
        <dl className="runs-facts">
          <div>
            <dt>Started</dt>
            <dd>{formatTimestamp(run.started_at ?? run.queued_at)}</dd>
          </div>
          <div>
            <dt>Trigger</dt>
            <dd>
              {run.trigger}
              {run.attempt > 1 && ` · attempt ${run.attempt}`}
            </dd>
          </div>
          <div title="User / system CPU of the direct process; may include waited-for descendants.">
            <dt>CPU user / sys</dt>
            <dd className="mono">{usage ? `${(usage.user_cpu_us / 1e6).toFixed(2)}s / ${(usage.system_cpu_us / 1e6).toFixed(2)}s` : '—'}</dd>
          </div>
          <div>
            <dt>Peak RSS</dt>
            <dd className="mono">{usage ? memoryMiB(usage.peak_rss_bytes) : '—'}</dd>
          </div>
          <div>
            <dt>Output</dt>
            <dd className="mono">{formatBytes(run.log_bytes)}</dd>
          </div>
        </dl>
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
      </div>
    </>
  );
}
