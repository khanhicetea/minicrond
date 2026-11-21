import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'wouter';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { RunGraph } from '../components/RunGraph';
import { api, errorText } from '../api';
import { LegendDot, LineChart, StackedBars, StepChart } from '../components/charts';
import { daemonQuery, runMetricsQuery } from '../queries';
import { useOverview } from '../lib/attention';
import { formatDurationMs, formatUptime } from '../lib/format';
import { useRange, RANGE_MS, RANGE_LABEL, type RangeKey } from '../lib/range';
import { jobPath } from '../lib/routes';
import type { RunJobMetrics, RunMetrics, WorkerState } from '../types';
import { useChartColors } from '../lib/tokens';

const BUCKETS = 48;
const RANGE_KEYS: RangeKey[] = ['15m', '1h', '24h', '7d', '30d'];
const EMPTY_METRICS: RunMetrics = { total: 0, succeeded: 0, failed: 0, active: 0, queued: 0, jobs: [], buckets: [] };

type WorkerGroup = WorkerState['status'];
const WORKER_GROUPS: { key: WorkerGroup; label: string; dot: string }[] = [
  { key: 'running', label: 'Running', dot: 'bg-ok' },
  { key: 'restarting', label: 'Restarting', dot: 'bg-warn' },
  { key: 'held', label: 'Held', dot: 'bg-warn' },
  { key: 'exhausted', label: 'Exhausted', dot: 'bg-danger' },
  { key: 'stopped', label: 'Stopped / disabled', dot: 'bg-neutral' },
];

function formatChartTimeLabel(time: number, range: RangeKey) {
  const date = new Date(time);
  if (range === '7d' || range === '30d') return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  if (range === '24h') return date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

const duration = (ms: number | null | undefined) => (ms == null ? '—' : formatDurationMs(ms));
const failureRate = (job: { succeeded: number; failed: number }) => {
  const done = job.succeeded + job.failed;
  return done > 0 ? (job.failed / done) * 100 : null;
};

/** Metrics — run statistics computed on request from stored history, plus daemon facts. */
export default function Metrics() {
  const { range, setRange } = useRange();
  const { info: BLUE, warn: AMBER, ok: GREEN, danger: RED } = useChartColors();
  const daemon = useQuery(daemonQuery());
  const metrics = useQuery(runMetricsQuery(range, BUCKETS));
  const alerts = useQuery({ queryKey: ['alert-metrics'], queryFn: api.alertMetrics, refetchInterval: 30_000, refetchIntervalInBackground: false });
  const { jobs, definitions, runsByJob, workerStates } = useOverview();
  const [filter, setFilter] = useState('');

  if (metrics.isPending) {
    return <div className="space-y-4"><PageHeader title="Metrics" /><div className="skeleton h-48 w-full" /></div>;
  }
  if (metrics.isError && !metrics.data) {
    return (
      <div className="space-y-4">
        <PageHeader title="Metrics" />
        <div role="alert" className="panel border-danger/40 bg-danger/5 px-4 py-3 text-sm text-danger-fg">
          Metrics are unavailable: {errorText(metrics.error)}.
          <button type="button" className="ml-3 underline" onClick={() => void metrics.refetch()}>Retry</button>
        </div>
      </div>
    );
  }

  const data = metrics.data ?? EMPTY_METRICS;
  const now = Date.now();
  const windowMs = RANGE_MS[range];
  const start = now - windowMs;
  const bucketMs = windowMs / BUCKETS;
  const buckets = data.buckets;
  const labels = buckets.map((_, i) => formatChartTimeLabel(start + i * bucketMs, range));
  const completed = data.succeeded + data.failed;
  const rate = failureRate(data);
  const failingJobs = data.jobs.filter(job => job.failed > 0).length;

  // Workers: every defined worker lands in exactly one group; disabled ones count as stopped.
  const workers = definitions.filter(d => d.kind === 'worker');
  const workerCounts: Record<WorkerGroup, number> = { running: 0, restarting: 0, held: 0, exhausted: 0, stopped: 0 };
  const restartingNames: string[] = [];
  for (const worker of workers) {
    const state = workerStates[worker.name];
    const group: WorkerGroup = worker.enabled === false || !state ? 'stopped' : state.status in workerCounts ? state.status : 'stopped';
    workerCounts[group] += 1;
    if (group === 'restarting') restartingNames.push(worker.name);
  }
  const workersUp = workerCounts.running;
  const workersTrouble = workerCounts.restarting + workerCounts.held + workerCounts.exhausted;

  const needle = filter.trim().toLowerCase();
  const jobRows = [...data.jobs]
    .filter(job => !needle || job.name.toLowerCase().includes(needle))
    .sort((a, b) => b.failed - a.failed || (failureRate(b) ?? 0) - (failureRate(a) ?? 0) || a.name.localeCompare(b.name));

  const history = daemon.data?.diagnostics?.run_history;
  const degraded = Boolean(history && (history.storage_degraded || history.read_only || history.dropped > 0));
  const alertCounts = Object.entries(alerts.data?.counts ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const stale = metrics.isError || jobs.isError || daemon.isError;

  return (
    <div className="space-y-5">
      {stale && (
        <div role="alert" className="panel border-warn/40 bg-warn/5 px-4 py-3 text-sm text-warn-fg">
          {metrics.isError ? 'Metrics' : jobs.isError ? 'Definitions' : 'Daemon status'} could not be refreshed. Displayed values may be stale.
          <button type="button" className="ml-3 underline" onClick={() => { void metrics.refetch(); void jobs.refetch(); void daemon.refetch(); }}>Retry</button>
        </div>
      )}
      <PageHeader
        title="Metrics"
        subtitle={`Run history for the ${RANGE_LABEL[range].toLowerCase()} · computed when this page is open`}
        actions={
          <div className="tab-seg" role="group" aria-label="Time range">
            {RANGE_KEYS.map(key => (
              <button key={key} type="button" aria-pressed={range === key} className={range === key ? 'active' : ''} onClick={() => setRange(key)}>
                {key}
              </button>
            ))}
          </div>
        }
      />

      {degraded && history && (
        <section role="alert" className="panel flex flex-wrap items-center gap-3 border-warn/40 bg-warn/5 px-4 py-3 text-sm">
          <Icon name="alert-triangle" size={16} className="shrink-0 text-warn-fg" />
          <strong className="font-semibold text-warn-fg">Run history is {history.read_only ? 'read-only' : 'degraded'}</strong>
          <span className="muted">
            Schedules keep running. {history.pending} runs waiting to be stored, {history.dropped} dropped. Figures below may be incomplete.
          </span>
        </section>
      )}

      {/* KPI row */}
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
        <Kpi label="Completed runs" value={completed.toLocaleString()} sub={`${data.total.toLocaleString()} recorded in window`} />
        <Kpi
          label="Failure rate"
          value={rate === null ? '—' : `${rate.toFixed(rate < 10 ? 1 : 0)}%`}
          valueClass={data.failed > 0 ? 'text-danger-fg' : ''}
          sub={data.failed > 0 ? <><span className="text-danger-fg">{data.failed} failed</span> in {failingJobs} {failingJobs === 1 ? 'job' : 'jobs'}</> : 'No failures'}
        />
        <Kpi label="Duration p95" value={duration(data.duration_p95_ms)} sub={`median ${duration(data.duration_p50_ms)}`} />
        <Kpi label="Running now" value={String(data.active)} valueClass={data.active > 0 ? 'text-link' : ''} sub={`${data.queued} queued`} />
        <Kpi
          label="Workers"
          value={<>{workersUp}<span className="text-base font-normal faint"> / {workers.length} up</span></>}
          sub={workersTrouble > 0 ? <span className="text-warn-fg">{workersTrouble} need attention</span> : workers.length === 0 ? 'None defined' : 'All healthy or stopped'}
        />
      </div>

      <div className="flex flex-wrap items-start gap-5">
        {/* Main column */}
        <div className="flex min-w-0 flex-[999_1_40rem] flex-col gap-5">
          <ChartPanel
            title="Completed runs"
            legend={<><LegendDot color={GREEN} label="Succeeded" /><LegendDot color={RED} label="Failed" /></>}
            right={<span className="num text-xs muted">{formatDurationMs(bucketMs)} per bar</span>}
          >
            <StackedBars buckets={buckets.map(b => ({ success: b.success, failure: b.failure }))} labels={labels} height={200} />
          </ChartPanel>

          <div className="grid grid-cols-1 gap-5 xl:grid-cols-2">
            <ChartPanel title="Run duration" legend={<><LegendDot color={BLUE} label="p50" /><LegendDot color={AMBER} label="p95" /></>}>
              <LineChart
                series={[
                  { name: 'p50', color: BLUE, points: buckets.map(b => (b.duration_p50_ms == null ? null : b.duration_p50_ms / 1000)) },
                  { name: 'p95', color: AMBER, points: buckets.map(b => (b.duration_p95_ms == null ? null : b.duration_p95_ms / 1000)) },
                ]}
                labels={labels}
                height={180}
                log
                formatY={value => `${value >= 1 ? Number(value.toFixed(2)) : value.toFixed(2)} s`}
              />
            </ChartPanel>
            <ChartPanel title="Concurrency" legend={<><LegendDot color={BLUE} label="Running" /><LegendDot color={AMBER} label="Queued" /></>}>
              <StepChart
                series={[
                  { name: 'Running', color: BLUE, points: buckets.map(b => b.active) },
                  { name: 'Queued', color: AMBER, points: buckets.map(b => b.queued) },
                ]}
                labels={labels}
                height={180}
              />
            </ChartPanel>
          </div>

          <section className="panel" aria-labelledby="metrics-jobs">
            <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3 sm:px-5">
              <div className="flex items-baseline gap-2">
                <h2 id="metrics-jobs" className="panel-title">Jobs</h2>
                <span className="text-xs faint">sorted by failures</span>
              </div>
              <label className="flex items-center gap-2 text-xs muted">
                <Icon name="search" size={13} />
                <span className="sr-only">Filter jobs</span>
                <input type="search" className="mc-input h-9 w-48 py-1 text-xs" placeholder="Filter jobs" value={filter} onChange={event => setFilter(event.target.value)} />
              </label>
            </div>
            <div className="overflow-x-auto">
              <table className="mc-table">
                <thead>
                  <tr>
                    <th>Job</th>
                    <th className="text-right">Runs</th>
                    <th className="min-w-[10rem]">Failures</th>
                    <th className="text-right">p50</th>
                    <th className="text-right">p95</th>
                    <th className="min-w-[9rem]">Recent runs</th>
                  </tr>
                </thead>
                <tbody>
                  {jobRows.map(job => <JobRow key={job.name} job={job} runs={runsByJob.get(job.name) ?? []} now={now} />)}
                  {jobRows.length === 0 && (
                    <tr><td colSpan={6} className="py-6 text-center text-xs faint">{needle ? 'No jobs match the filter.' : 'No runs in the selected window.'}</td></tr>
                  )}
                </tbody>
              </table>
            </div>
          </section>
        </div>

        {/* Rail */}
        <aside aria-label="System" className="flex min-w-0 flex-[1_1_18rem] flex-col gap-5">
          <section className="panel p-4 sm:p-5" aria-labelledby="metrics-workers">
            <div className="mb-3 flex items-baseline justify-between">
              <h2 id="metrics-workers" className="panel-title">Workers</h2>
              <span className="num text-xs faint">{workers.length} defined</span>
            </div>
            {workers.length > 0 && (
              <div className="mb-3 flex h-2 w-full gap-0.5 overflow-hidden rounded-full bg-base-200">
                {WORKER_GROUPS.map(group => workerCounts[group.key] > 0 && (
                  <div key={group.key} className={group.dot} style={{ width: `${(workerCounts[group.key] / workers.length) * 100}%` }} />
                ))}
              </div>
            )}
            <ul className="space-y-2 text-sm">
              {WORKER_GROUPS.map(group => (
                <li key={group.key} className="flex items-center gap-2">
                  <span className={`inline-block h-2 w-2 rounded-full ${group.dot}`} />
                  {group.label}
                  {group.key === 'restarting' && restartingNames.length > 0 && <span className="truncate text-xs faint">· {restartingNames.join(', ')}</span>}
                  <span className={`num ml-auto ${workerCounts[group.key] === 0 ? 'faint' : ''}`}>{workerCounts[group.key]}</span>
                </li>
              ))}
            </ul>
            {workers.length === 0 && <p className="mt-3 text-xs faint">{jobs.isPending ? 'Loading workers…' : 'No workers defined.'}</p>}
          </section>

          <section className="panel p-4 sm:p-5" aria-labelledby="metrics-alerts">
            <div className="mb-3 flex items-baseline justify-between">
              <h2 id="metrics-alerts" className="panel-title">Alert deliveries</h2>
              <span className="num text-xs faint">queue {alerts.data?.queue_depth ?? '—'}</span>
            </div>
            {alertCounts.length > 0 ? (
              <dl className="grid grid-cols-[1fr_auto] gap-x-3 gap-y-2 text-sm">
                {alertCounts.map(([status, count]) => {
                  const bad = status === 'failed' || status === 'dropped';
                  return (
                    <div key={status} className="contents">
                      <dt className={bad && count > 0 ? 'text-danger-fg' : 'muted'}>{status}</dt>
                      <dd className={`num text-right ${bad && count > 0 ? 'text-danger-fg' : ''}`}>{count}</dd>
                    </div>
                  );
                })}
              </dl>
            ) : (
              <p className="text-xs faint">{alerts.isError ? 'Alert metrics are unavailable.' : alerts.isPending ? 'Loading…' : 'No alerts delivered yet.'}</p>
            )}
            <Link href="/settings" className="mt-3 inline-block text-sm text-link hover:text-link-hover">Alert channels →</Link>
          </section>

          <section className="panel p-4 sm:p-5" aria-labelledby="metrics-daemon">
            <div className="mb-3 flex items-center justify-between">
              <h2 id="metrics-daemon" className="panel-title">Daemon</h2>
              {daemon.data && (
                <span className={`flex items-center gap-1.5 text-xs ${degraded ? 'text-warn-fg' : 'text-ok-fg'}`}>
                  <span className={`dot ${degraded ? 'dot-amber' : 'dot-green'}`} />
                  {degraded ? 'Degraded' : 'Healthy'}
                </span>
              )}
            </div>
            {daemon.data ? (
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
                <dt className="muted">Uptime</dt><dd className="num text-right">{formatUptime(daemon.data.uptime_s)}</dd>
                <dt className="muted">Version</dt><dd className="num text-right">v{daemon.data.version} · schema {daemon.data.schema_version}</dd>
                <dt className="muted">Run history</dt>
                <dd className={`text-right ${history && history.pending > 0 ? 'text-warn-fg' : ''}`}>
                  {!history ? '—' : history.pending > 0 ? `background writer · ${history.pending} pending` : 'up to date'}
                </dd>
                {daemon.data.timezone && <><dt className="muted">Time zone</dt><dd className="num text-right">{daemon.data.timezone}</dd></>}
              </dl>
            ) : (
              <p className="text-xs faint">{daemon.isError ? 'Daemon status is unavailable.' : 'Connecting…'}</p>
            )}
            <Link href="/monitor" className="mt-3 inline-block text-sm text-link hover:text-link-hover">Live CPU and memory in Monitor →</Link>
          </section>

          <p className="text-xs leading-relaxed faint">
            Figures come from stored run history and are computed only while this page is open; nothing is sampled in the background.
          </p>
        </aside>
      </div>
    </div>
  );
}

function Kpi({ label, value, sub, valueClass = '' }: { label: string; value: React.ReactNode; sub?: React.ReactNode; valueClass?: string }) {
  return (
    <div className="stat-card">
      <span className="label">{label}</span>
      <div className={`num text-[1.75rem] font-semibold leading-tight ${valueClass}`}>{value}</div>
      {sub && <div className="text-xs muted">{sub}</div>}
    </div>
  );
}

function JobRow({ job, runs, now }: { job: RunJobMetrics; runs: Parameters<typeof RunGraph>[0]['runs']; now: number }) {
  const rate = failureRate(job);
  return (
    <tr>
      <td className="max-w-[16rem] truncate">
        <Link href={jobPath(job.name)} className="flex items-center gap-2 font-medium hover:text-link-hover">
          <span className={`inline-block h-2 w-2 shrink-0 rounded-full ${job.failed > 0 ? 'bg-danger' : 'bg-ok'}`} />
          <span className="truncate">{job.name}</span>
          {job.active > 0 && <span className="chip chip-info !px-1.5 !py-0 text-[0.65rem]">{job.active} running</span>}
        </Link>
      </td>
      <td className="num text-right">{job.total}</td>
      <td>
        <div className="flex items-center gap-2.5">
          <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-base-200">
            <div className="h-full bg-danger" style={{ width: `${Math.min(100, rate ?? 0)}%` }} />
          </div>
          <span className={`num min-w-[4.5rem] text-right ${job.failed > 0 ? 'text-danger-fg' : 'faint'}`}>
            {job.failed === 0 ? '0' : `${job.failed} · ${rate === null ? '—' : `${rate.toFixed(rate < 10 ? 1 : 0)}%`}`}
          </span>
        </div>
      </td>
      <td className="num text-right">{duration(job.duration_p50_ms)}</td>
      <td className="num text-right">{duration(job.duration_p95_ms)}</td>
      <td><RunGraph runs={runs} now={now} /></td>
    </tr>
  );
}

function ChartPanel({ title, right, legend, children }: { title: string; right?: React.ReactNode; legend?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="panel p-4 sm:p-5">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="panel-title">{title}</h2>
        <div className="flex flex-wrap items-center gap-3">
          {legend}
          {right}
        </div>
      </div>
      {children}
    </section>
  );
}
