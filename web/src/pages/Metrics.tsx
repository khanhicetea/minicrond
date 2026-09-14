import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'wouter';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { LegendDot, LineChart, StackedBars, StepChart } from '../components/charts';
import { daemonQuery, jobsQuery, runMetricsQuery, useWorkerStates } from '../queries';
import { formatDurationMs, formatUptime } from '../lib/format';
import { useRange, RANGE_MS, RANGE_LABEL, type RangeKey } from '../lib/range';
import { jobPath } from '../lib/routes';
import type { RunMetrics } from '../types';

const BLUE = '#60a5fa';
const GREEN = '#34d399';
const RED = '#f87171';
const AMBER = '#fbbf24';
const BUCKETS = 48;

const EMPTY_METRICS: RunMetrics = { total: 0, succeeded: 0, failed: 0, active: 0, queued: 0, jobs: [], buckets: [] };

function formatChartTimeLabel(time: number, range: RangeKey) {
  const date = new Date(time);
  if (range === '7d' || range === '30d') {
    return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
  }
  if (range === '24h') {
    return date.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  }
  return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
}

/** Metrics — run statistics computed from live history + daemon info. */
export default function Metrics() {
  const { range, setRange } = useRange();
  const daemon = useQuery(daemonQuery());
  const metrics = useQuery(runMetricsQuery(range, BUCKETS));
  const jobs = useQuery(jobsQuery());
  const [jobFilter, setJobFilter] = useState('');

  const jobNames = (jobs.data ?? []).filter(d => d.kind === 'job').map(d => d.name).sort();
  const workerNames = (jobs.data ?? []).filter(d => d.kind === 'worker').map(d => d.name);
  const workerStates = useWorkerStates(workerNames);

  const now = Date.now();
  const windowMs = RANGE_MS[range];

  const data = metrics.data ?? EMPTY_METRICS;
  const stats = {
    ...data,
    rate: data.total > 0 ? (data.succeeded / data.total) * 100 : null,
    median: data.duration_p50_ms ?? null,
    p95: data.duration_p95_ms ?? null,
  };

  // Per-job aggregates for the Job stats panel.
  const jobStats = stats.jobs.map(job => ({
    ...job,
    rate: job.total > 0 ? (job.succeeded / job.total) * 100 : null,
    median: job.duration_p50_ms ?? null,
    p95: job.duration_p95_ms ?? null,
  }));

  const selectedJob = jobFilter === '' ? null : jobStats.find(s => s.name === jobFilter) ?? null;

  // Bucket the window for charts.
  const start = now - windowMs;
  const bucketMs = windowMs / BUCKETS;
  const buckets = stats.buckets;
  const success = buckets.map(bucket => bucket.success);
  const failure = buckets.map(bucket => bucket.failure);
  const median = buckets.map(bucket => bucket.duration_p50_ms ?? null);
  const p95 = buckets.map(bucket => bucket.duration_p95_ms ?? null);
  const activeSeries = buckets.map(bucket => bucket.active);
  const queueSeries = buckets.map(bucket => bucket.queued);
  const timeLabels = buckets.map((_, i) => formatChartTimeLabel(start + i * bucketMs, range));
  const charts = { success, failure, median, p95, activeSeries, queueSeries, timeLabels };

  const rows = { healthy: 0, attention: 0, stopped: 0 };
  for (const name of workerNames) {
    const state = workerStates[name];
    if (!state) continue;
    if ((state.failures ?? 0) > 0 || state.held) rows.attention += 1;
    else if (state.active) rows.healthy += 1;
    else rows.stopped += 1;
  }
  const total = rows.healthy + rows.attention + rows.stopped;
  const workerRows = { rows, total };

  const rangeKeys: RangeKey[] = ['15m', '1h', '24h', '7d', '30d'];
  const activeRange = range;

  return (
    <div className="space-y-4">
      <PageHeader
        title="Metrics"
        subtitle={
          <span className="inline-flex items-center gap-1.5">
            Computed by backend run metrics · updated live <span className="dot dot-green dot-pulse" />
          </span>
        }
        actions={
          <div className="tab-seg">
            {rangeKeys.map(key => (
              <button key={key} type="button" className={activeRange === key ? 'active' : ''} onClick={() => setRange(key)}>
                {key}
              </button>
            ))}
          </div>
        }
      />

      {/* Stat cards */}
      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard icon="activity" label={`Runs (${RANGE_LABEL[activeRange].replace('Last ', '')})`} value={String(stats.total)} sub="Total runs queued" />
        <StatCard
          icon="trending-up"
          label="Success rate"
          value={stats.rate === null ? '—' : `${stats.rate.toFixed(1)}%`}
          valueClass="text-green-400"
          sub={
            <>
              <span className="text-green-400">{stats.succeeded} succeeded</span> · <span className="text-red-400">{stats.failed} failed</span>
            </>
          }
        />
        <StatCard icon="users" label="Active runs" value={String(stats.active)} valueClass={stats.active > 0 ? 'text-blue-400' : ''} sub="Currently executing" />
        <StatCard icon="file-text" label="Daemon uptime" value={daemon.data ? formatUptime(daemon.data.uptime_s) : '—'} valueClass="text-amber-400" sub={daemon.data ? `v${daemon.data.version} · schema ${daemon.data.schema_version}` : 'connecting…'} />
      </div>

      {/* Main 4/8 split */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-12">
        {/* Left column: Worker states, Active runs, Run stats */}
        <div className="flex min-w-0 flex-col gap-4 lg:col-span-4">
          {/* Worker states */}
          <section className="panel p-4 sm:p-5">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="panel-title">Worker states</h2>
              <span className="num text-xs muted">{workerRows.total} total</span>
            </div>
            {workerRows.total > 0 && (
              <div className="mb-3 flex h-2 w-full overflow-hidden rounded-full bg-base-200" title="Healthy / needs attention / stopped">
                <div className="bg-green-400" style={{ width: `${(workerRows.rows.healthy / workerRows.total) * 100}%` }} />
                <div className="bg-amber-400" style={{ width: `${(workerRows.rows.attention / workerRows.total) * 100}%` }} />
                <div className="bg-red-400" style={{ width: `${(workerRows.rows.stopped / workerRows.total) * 100}%` }} />
              </div>
            )}
            <table className="mc-table">
              <thead>
                <tr>
                  <th>State</th>
                  <th className="text-right">Instances</th>
                  <th className="text-right">%</th>
                </tr>
              </thead>
              <tbody>
                <WorkerRow color="bg-green-400" label="Healthy" count={workerRows.rows.healthy} total={workerRows.total} />
                <WorkerRow color="bg-amber-400" label="Needs attention" count={workerRows.rows.attention} total={workerRows.total} />
                <WorkerRow color="bg-red-400" label="Stopped" count={workerRows.rows.stopped} total={workerRows.total} />
              </tbody>
            </table>
            {workerRows.total === 0 && <p className="mt-3 text-xs faint">No workers defined.</p>}
          </section>

          {/* Active runs & queue depth */}
          <ChartPanel
            title="Active runs"
            right={
              <span className="num text-xs">
                <span className="text-blue-400">Active {stats.active}</span>
                <span className="mx-2 faint">·</span>
                <span className="text-amber-400">Queued {stats.queued}</span>
              </span>
            }
            legend={
              <>
                <LegendDot color={BLUE} label="Active" />
                <LegendDot color={AMBER} label="Queued" />
              </>
            }
          >
            <StepChart
              series={[
                { name: 'Active', color: BLUE, points: charts.activeSeries },
                { name: 'Queued', color: AMBER, points: charts.queueSeries },
              ]}
              labels={charts.timeLabels}
              height={180}
            />
          </ChartPanel>

          {/* Run statistics */}
          <section className="panel p-4 sm:p-5">
            <div className="mb-3 flex items-center justify-between">
              <h2 className="panel-title">Run stats</h2>
              <span className="text-xs muted">{RANGE_LABEL[activeRange]}</span>
            </div>
            <table className="mc-table">
              <thead>
                <tr>
                  <th>Statistic</th>
                  <th className="text-right">Value</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.queued</td>
                  <td className="num text-right">{stats.total}</td>
                </tr>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.succeeded</td>
                  <td className="num text-right text-green-400">{stats.succeeded}</td>
                </tr>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.failed</td>
                  <td className="num text-right text-red-400">{stats.failed}</td>
                </tr>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.duration_p50</td>
                  <td className="num text-right">{stats.median === null ? '—' : formatDurationMs(stats.median)}</td>
                </tr>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.duration_p95</td>
                  <td className="num text-right">{stats.p95 === null ? '—' : formatDurationMs(stats.p95)}</td>
                </tr>
                <tr>
                  <td className="font-mono text-[0.8rem]">runs.active</td>
                  <td className="num text-right">{stats.active}</td>
                </tr>
              </tbody>
            </table>
          </section>
        </div>

        {/* Right column: Job stats (filterable), then Run duration + Completed runs */}
        <div className="flex min-w-0 flex-col gap-4 lg:col-span-8">
          {/* Job stats */}
          <section className="panel p-4 sm:p-5">
            <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
              <h2 className="panel-title">Job stats</h2>
              <label className="flex items-center gap-2 text-xs muted">
                <Icon name="list" size={13} />
                <select className="mc-select h-9 py-1 text-xs" value={jobFilter} onChange={event => setJobFilter(event.target.value)} aria-label="Filter job stats">
                  <option value="">All jobs</option>
                  {jobNames.map(name => (
                    <option key={name} value={name}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
            </div>

            {jobFilter === '' ? (
              /* All jobs: per-job breakdown */
              <div className="overflow-x-auto">
                <table className="mc-table">
                  <thead>
                    <tr>
                      <th>Job</th>
                      <th className="text-right">Runs</th>
                      <th className="text-right">Success</th>
                      <th className="text-right">Failed</th>
                      <th className="text-right">Rate</th>
                      <th className="text-right">Median</th>
                      <th className="text-right">p95</th>
                    </tr>
                  </thead>
                  <tbody>
                    {jobStats.map(job => (
                      <tr key={job.name}>
                        <td className="max-w-[14rem] truncate">
                          <Link href={jobPath(job.name)} className="flex items-center gap-2 font-medium hover:text-blue-400">
                            <span className={`inline-block h-2 w-2 rounded-full ${job.failed > 0 ? 'bg-red-400' : 'bg-green-400'}`} />
                            {job.name}
                            {job.active > 0 && <span className="chip chip-info !px-1.5 !py-0 text-[0.65rem]">{job.active} active</span>}
                          </Link>
                        </td>
                        <td className="num text-right">{job.total}</td>
                        <td className="num text-right text-green-400">{job.succeeded}</td>
                        <td className="num text-right text-red-400">{job.failed}</td>
                        <td className="num text-right">{job.rate === null ? '—' : `${job.rate.toFixed(0)}%`}</td>
                        <td className="num text-right">{job.median === null ? '—' : formatDurationMs(job.median)}</td>
                        <td className="num text-right">{job.p95 === null ? '—' : formatDurationMs(job.p95)}</td>
                      </tr>
                    ))}
                    {jobStats.length === 0 && (
                      <tr>
                        <td colSpan={7} className="py-6 text-center text-xs faint">
                          No runs in the selected window.
                        </td>
                      </tr>
                    )}
                  </tbody>
                </table>
              </div>
            ) : selectedJob ? (
              /* Single job: summary strip */
              <div className="space-y-3">
                <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
                  <MiniStat label="Runs" value={String(selectedJob.total)} />
                  <MiniStat label="Succeeded" value={String(selectedJob.succeeded)} valueClass="text-green-400" />
                  <MiniStat label="Failed" value={String(selectedJob.failed)} valueClass="text-red-400" />
                  <MiniStat label="Active" value={String(selectedJob.active)} valueClass={selectedJob.active > 0 ? 'text-blue-400' : ''} />
                  <MiniStat label="Median" value={selectedJob.median === null ? '—' : formatDurationMs(selectedJob.median)} />
                  <MiniStat label="p95" value={selectedJob.p95 === null ? '—' : formatDurationMs(selectedJob.p95)} />
                </div>
                <div>
                  <div className="mb-1 flex items-center justify-between text-xs muted">
                    <span>
                      Success rate{' '}
                      <span className="num font-medium text-base-content">{selectedJob.rate === null ? '—' : `${selectedJob.rate.toFixed(1)}%`}</span>
                    </span>
                    <span className="num">
                      <span className="text-green-400">{selectedJob.succeeded}</span> / <span className="text-red-400">{selectedJob.failed}</span>
                    </span>
                  </div>
                  <div className="flex h-2 w-full overflow-hidden rounded-full bg-base-200">
                    <div className="bg-green-400" style={{ width: `${selectedJob.rate ?? 0}%` }} />
                    <div className="bg-red-400" style={{ width: `${selectedJob.failed + selectedJob.succeeded > 0 ? 100 - (selectedJob.rate ?? 0) : 0}%` }} />
                  </div>
                </div>
              </div>
            ) : (
              <p className="py-6 text-center text-xs faint">No runs for this job in the selected window.</p>
            )}
          </section>

          {/* Run duration + Completed runs */}
          <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
            <ChartPanel
              title="Run duration"
              right={
                <span className="num text-xs">
                  <span className="text-blue-400">p50</span> {stats.median === null ? '—' : formatDurationMs(stats.median)}
                  <span className="mx-2 faint">·</span>
                  <span className="text-green-400">p95</span> {stats.p95 === null ? '—' : formatDurationMs(stats.p95)}
                </span>
              }
              legend={
                <>
                  <LegendDot color={BLUE} label="p50" />
                  <LegendDot color={GREEN} label="p95" />
                </>
              }
            >
              <LineChart
                series={[
                  { name: 'p50', color: BLUE, points: charts.median.map(ms => (ms === null ? null : ms / 1000)) },
                  { name: 'p95', color: GREEN, points: charts.p95.map(ms => (ms === null ? null : ms / 1000)) },
                ]}
                labels={charts.timeLabels}
                log
                formatY={value => (value >= 1 ? String(Number(value.toFixed(2))) : value.toFixed(2))}
              />
            </ChartPanel>

            <ChartPanel
              title="Completed runs"
              right={<span className="num text-xs muted">Total {stats.succeeded + stats.failed}</span>}
              legend={
                <>
                  <LegendDot color={GREEN} label="Success" />
                  <LegendDot color={RED} label="Failure" />
                </>
              }
            >
              <StackedBars
                buckets={charts.success.map((value, index) => ({ success: value, failure: charts.failure[index] }))}
                labels={charts.timeLabels}
              />
            </ChartPanel>
          </div>
        </div>
      </div>
    </div>
  );
}

function StatCard({ icon, label, value, sub, valueClass = '' }: { icon: 'activity' | 'trending-up' | 'users' | 'file-text'; label: string; value: string; sub?: React.ReactNode; valueClass?: string }) {
  return (
    <div className="stat-card">
      <div className="flex items-center justify-between">
        <span className="label">{label}</span>
        <Icon name={icon} size={16} className="text-blue-400" />
      </div>
      <div className={`text-[1.9rem] font-bold leading-tight ${valueClass}`}>{value}</div>
      {sub && <div className="text-xs muted">{sub}</div>}
    </div>
  );
}

function MiniStat({ label, value, valueClass = '' }: { label: string; value: string; valueClass?: string }) {
  return (
    <div className="rounded-lg border border-base-300 bg-base-200 px-3 py-2">
      <div className="text-[0.65rem] uppercase tracking-wide muted">{label}</div>
      <div className={`num text-lg font-semibold leading-tight ${valueClass}`}>{value}</div>
    </div>
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

function WorkerRow({ color, label, count, total }: { color: string; label: string; count: number; total: number }) {
  return (
    <tr>
      <td>
        <span className="flex items-center gap-2">
          <span className={`inline-block h-2 w-2 rounded-full ${color}`} />
          {label}
        </span>
      </td>
      <td className="num text-right">{count}</td>
      <td className="num text-right">{total > 0 ? `${((count / total) * 100).toFixed(1)}%` : '—'}</td>
    </tr>
  );
}
