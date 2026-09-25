import { useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useInfiniteQuery, useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import LogViewer from '../components/LogViewer';
import { RunStatusDot } from '../components/RunsTable';
import { api, errorText } from '../api';
import { downloadFile } from '../lib/download';
import { formatBytes, formatDayTime, formatSpan, formatTimestamp, shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';
import { jobsQuery, runQuery, useStopRun, useTriggerJob } from '../queries';
import { isActiveRun, type Run } from '../types';

type FilterKey = 'all' | 'active' | 'failed' | 'scheduled' | 'manual';
const FILTERS: { key: FilterKey; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'active', label: 'Active' },
  { key: 'failed', label: 'Failed' },
  { key: 'scheduled', label: 'Scheduled' },
  { key: 'manual', label: 'Manual' },
];

function matchesFilter(run: Run, filter: FilterKey): boolean {
  if (filter === 'active') return isActiveRun(run);
  if (filter === 'failed') return ['failed', 'timeout', 'interrupted'].includes(run.status);
  if (filter === 'scheduled') return run.trigger === 'schedule';
  if (filter === 'manual') return run.trigger === 'manual';
  return true;
}

/** Job → run history → live output, all within the Runs tab. */
export default function Runs() {
  const search = useSearch();
  const [, navigate] = useLocation();
  const params = new URLSearchParams(search);
  const requestedJob = params.get('job');
  const requestedRun = params.get('run');
  const filterParam = params.get('filter');
  const filter: FilterKey = FILTERS.some(item => item.key === filterParam) ? filterParam as FilterKey : 'all';
  const [jobSearch, setJobSearch] = useState('');
  const [actionError, setActionError] = useState('');
  const jobs = useQuery(jobsQuery());
  const definitions = [...(jobs.data ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const selectedJob = definitions.find(job => job.name === requestedJob)?.name ?? (requestedJob ? '' : definitions[0]?.name ?? '');
	const configOwned = definitions.find(job => job.name === selectedJob)?.source === 'config';
  const history = useInfiniteQuery({
    queryKey: ['run-pages', selectedJob, filter],
    queryFn: ({ pageParam, signal }) => api.listRunsPage(selectedJob, 100, pageParam, filter === 'all' ? '' : filter, signal),
    initialPageParam: '',
    getNextPageParam: page => page.next_before || undefined,
    enabled: Boolean(selectedJob),
    // Refresh even when no run is active so scheduled and externally triggered
    // runs appear without navigating away. React Query stops on unmount.
    refetchInterval: 2_000,
  });
  const filtered = history.data?.pages.flatMap(page => page.items ?? []) ?? [];
  const runId = requestedRun || filtered[0]?.run_id || '';
  const detail = useQuery({ ...runQuery(runId), enabled: Boolean(selectedJob && runId) });
  // A freshly triggered run may not have appeared in the history response yet.
  const selectedRun = filtered.find(run => run.run_id === runId) ??
    (detail.data?.job === selectedJob && matchesFilter(detail.data, filter) ? detail.data : undefined);
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

  return (
    <div className="runs-workspace" aria-label="Run history">
      <aside className="runs-pane" aria-label="Jobs and workers">
        <div className="runs-pane-header">
          <div className="flex items-center justify-between gap-2">
            <h1 className="panel-title">Jobs &amp; workers</h1>
            <span className="num text-xs faint">{definitions.length}</span>
          </div>
          <label className="mt-3 flex items-center gap-2 rounded-lg border border-base-300 bg-base-200 px-2.5 py-1.5">
            <Icon name="search" size={14} className="faint" />
            <input className="min-w-0 w-full bg-transparent text-xs outline-none" value={jobSearch} onChange={e => setJobSearch(e.target.value)} placeholder="Find a job…" aria-label="Find a job" />
          </label>
        </div>
        <div className="runs-pane-scroll p-2">
          {jobs.isPending && <p className="p-3 text-sm muted">Loading jobs…</p>}
          {jobs.isError && <p role="alert" className="p-3 text-sm text-red-400">{errorText(jobs.error)}</p>}
          {!jobs.isPending && definitions.length === 0 && <p className="p-3 text-sm muted">No jobs or workers yet.</p>}
          {definitions.filter(job => job.name.toLowerCase().includes(jobSearch.trim().toLowerCase())).map(job => (
            <button key={job.name} type="button" className={`runs-job-row ${selectedJob === job.name ? 'selected' : ''}`} onClick={() => go(job.name)} aria-pressed={selectedJob === job.name}>
              <Icon name={job.kind === 'worker' ? 'terminal' : 'calendar'} size={15} className="shrink-0 muted" />
              <span className="min-w-0 truncate font-mono text-xs" title={job.name}>{job.name}</span>
            </button>
          ))}
          {definitions.length > 0 && !definitions.some(job => job.name.toLowerCase().includes(jobSearch.trim().toLowerCase())) && <p className="p-3 text-sm muted">No matching jobs.</p>}
        </div>
      </aside>

      <section className="runs-pane" aria-label="Runs for selected job">
        <div className="runs-pane-header">
          <div className="flex min-w-0 items-center justify-between gap-2">
            <div className="min-w-0">
              <h2 className="panel-title">Run history</h2>
              <p className="mt-0.5 truncate font-mono text-xs muted" title={selectedJob}>{selectedJob || 'Select a job'}</p>
            </div>
            <span className="num shrink-0 text-xs faint">{filtered.length} loaded</span>
          </div>
          <div className="mt-3 flex flex-wrap gap-1" aria-label="Filter runs">
            {FILTERS.map(item => (
              <button key={item.key} type="button" className={`pill !px-2 !py-1 !text-xs ${filter === item.key ? 'active' : ''}`} onClick={() => go(selectedJob, '', item.key)} aria-pressed={filter === item.key}>{item.label}</button>
            ))}
          </div>
        </div>
        <div className="runs-pane-scroll p-2">
          {!selectedJob && <p className="p-3 text-sm muted">Select a job to see its runs.</p>}
          {selectedJob && history.isPending && <p className="p-3 text-sm muted">Loading runs…</p>}
          {history.isError && <p role="alert" className="p-3 text-sm text-red-400">{errorText(history.error)}</p>}
          {history.isSuccess && filtered.length === 0 && <p className="p-3 text-sm muted">No runs match this filter.</p>}
          {filtered.map(run => (
            <button key={run.run_id} type="button" className={`runs-history-row ${selectedRun?.run_id === run.run_id ? 'selected' : ''}`} onClick={() => go(selectedJob, run.run_id)} aria-pressed={selectedRun?.run_id === run.run_id}>
              <RunStatusDot status={run.status} />
              <span className="min-w-0 flex-1">
                <span className="flex items-center justify-between gap-2">
                  <span className="truncate font-mono text-xs font-semibold">{formatDayTime(run.started_at ?? run.queued_at)}</span>
                  <span className="num shrink-0 text-xs muted">{isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at)}</span>
                </span>
                <span className="mt-1 flex items-center justify-between gap-2 text-xs">
                  <span className="capitalize muted">{run.status} <span className="faint">·</span> {run.trigger}</span>
                  <span className="font-mono faint">#{shortRunId(run.run_id)}</span>
                </span>
              </span>
            </button>
          ))}
          {history.hasNextPage && (
            <button type="button" className="btn-sub mx-2 my-3" disabled={history.isFetchingNextPage} onClick={() => void history.fetchNextPage()}>
              {history.isFetchingNextPage ? 'Loading…' : 'Load older runs'}
            </button>
          )}
        </div>
      </section>

      <section className="runs-pane runs-output" aria-label="Selected run output">
        {!selectedRun ? (
          <div className="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-center muted">
            <Icon name="terminal" size={24} className="faint" />
            <p>Select a run to view its output.</p>
          </div>
        ) : detail.isPending ? (
          <div className="p-5 text-sm muted">Loading run output…</div>
        ) : detail.isError ? (
          <div role="alert" className="p-5 text-sm text-red-400">Failed to load run: {errorText(detail.error)}</div>
        ) : (
          <>
            <div className="runs-output-header">
              <div className="min-w-0">
                <div className="flex items-center gap-2">
                  <RunStatusDot status={detail.data.status} />
                  <h2 className="truncate text-base font-semibold capitalize">{detail.data.status}</h2>
                  <span className="num text-sm muted">{isActiveRun(detail.data) ? formatSpan(detail.data.started_at ?? detail.data.queued_at) : formatSpan(detail.data.started_at, detail.data.ended_at)}</span>
                </div>
                <p className="mt-1 truncate text-xs muted" title={detail.data.run_id}>
                  {formatTimestamp(detail.data.started_at ?? detail.data.queued_at)} · {detail.data.trigger} · <span className="font-mono">#{shortRunId(detail.data.run_id)}</span>
                </p>
              </div>
              <div className="flex shrink-0 flex-wrap items-center gap-1.5">
                {isActiveRun(detail.data) ? (
                  <button type="button" className="btn-danger-x" disabled={stop.isPending} onClick={() => { setActionError(''); stop.mutate(detail.data.run_id, { onError: err => setActionError(errorText(err)) }); }}><Icon name="square" size={13} />Stop</button>
                ) : !configOwned ? (
                  <button type="button" className="btn-sub" disabled={trigger.isPending} onClick={rerun}><Icon name="play" size={13} />Run again</button>
                ) : null}
                <button type="button" className="btn-icon" title="Download raw log" aria-label="Download raw log" onClick={() => { setActionError(''); downloadFile(api.rawLogUrl(detail.data.run_id), `${detail.data.run_id}.log`, api.download).catch(err => setActionError(errorText(err))); }}><Icon name="download" size={16} /></button>
                <Link href={`/runs/${detail.data.run_id}`} className="btn-icon" title="Full run details" aria-label="Full run details"><Icon name="external-link" size={16} /></Link>
              </div>
            </div>
            {actionError && <p role="alert" className="border-b border-base-300 px-4 py-2 text-xs text-red-400">{actionError}</p>}
            {detail.data.log_truncated && <p role="alert" className="border-b border-base-300 px-4 py-2 text-xs text-amber-400">Stored log exceeded the size cap and was truncated.</p>}
            <div className="flex min-h-0 flex-1 flex-col">
              <div className="flex items-center justify-between border-b border-base-300 px-4 py-2 text-xs">
                <span className="flex items-center gap-2 font-semibold"><Icon name="terminal" size={14} />Console output</span>
                <span className="num faint">{formatBytes(detail.data.log_bytes)}</span>
              </div>
              <LogViewer key={detail.data.run_id} runId={detail.data.run_id} />
            </div>
            <div className="border-t border-base-300 px-4 py-2 text-xs muted">
              <Link href={jobPath(selectedJob)} className="text-sky-300 hover:text-sky-200">View job details</Link>
              {detail.data.exit_code !== undefined && <span className="ml-3">Exit code: {detail.data.exit_code}</span>}
            </div>
          </>
        )}
      </section>
    </div>
  );
}
