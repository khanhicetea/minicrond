import { useMemo } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { LegendDot, LineChart, StackedBars, StepChart } from '../components/charts';
import { daemonQuery, jobsQuery, runsQuery, useWorkerStates } from '../queries';
import { formatDurationMs, formatUptime } from '../lib/format';
import { useRange, RANGE_MS, RANGE_LABEL, type RangeKey } from '../lib/range';
import { isActiveRun } from '../types';
import type { Run } from '../types';

const BLUE = '#60a5fa';
const GREEN = '#34d399';
const RED = '#f87171';
const AMBER = '#fbbf24';
const BUCKETS = 48;

/** Metrics — run statistics computed from live history + daemon info. */
export default function Metrics() {
  const { range, setRange } = useRange();
  const daemon = useQuery(daemonQuery());
  const runs = useQuery(runsQuery('', 500));
  const jobs = useQuery(jobsQuery());

  const workerNames = useMemo(() => (jobs.data ?? []).filter(d => d.kind === 'worker').map(d => d.name), [jobs.data]);
  const workerStates = useWorkerStates(workerNames);

  const now = Date.now();
  const windowMs = RANGE_MS[range];
  const inWindow = (run: Run) => new Date(run.queued_at).getTime() >= now - windowMs;

  const stats = useMemo(() => {
    const items = (runs.data ?? []).filter(inWindow);
    const completed = items.filter(run => !isActiveRun(run));
    const succeeded = items.filter(run => run.status === 'succeeded').length;
    const failed = items.filter(run => ['failed', 'timeout', 'interrupted'].includes(run.status)).length;
    const active = items.filter(isActiveRun).length;
    const rate = items.length > 0 ? (succeeded / items.length) * 100 : null;
    const durations = completed
      .map(run => {
        if (!run.started_at || !run.ended_at) return null;
        return new Date(run.ended_at).getTime() - new Date(run.started_at).getTime();
      })
      .filter((ms): ms is number => ms !== null && ms >= 0)
      .sort((a, b) => a - b);
    const percentile = (p: number) => (durations.length ? durations[Math.min(durations.length - 1, Math.floor(durations.length * p))] : null);
    return { items, succeeded, failed, active, rate, median: percentile(0.5), p95: percentile(0.95), total: items.length };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runs.data, range, now]);

  // Bucket the window for charts.
  const charts = useMemo(() => {
    const items = stats.items;
    const start = now - windowMs;
    const bucketMs = windowMs / BUCKETS;
    const bucketIndex = (ms: number) => Math.min(BUCKETS - 1, Math.max(0, Math.floor((ms - start) / bucketMs)));

    const success = Array.from({ length: BUCKETS }, () => 0);
    const failure = Array.from({ length: BUCKETS }, () => 0);
    const durations: number[][] = Array.from({ length: BUCKETS }, () => []);
    for (const run of items) {
      const ended = run.ended_at ? new Date(run.ended_at).getTime() : null;
      if (ended !== null) {
        const index = bucketIndex(ended);
        if (run.status === 'succeeded') success[index] += 1;
        else failure[index] += 1;
        if (run.started_at) {
          const duration = ended - new Date(run.started_at).getTime();
          if (duration >= 0) durations[index].push(duration);
        }
      }
    }
    const percentile = (values: number[], p: number) => {
      if (values.length === 0) return null;
      const sorted = [...values].sort((a, b) => a - b);
      return sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * p))];
    };
    const p50 = durations.map(values => percentile(values, 0.5));
    const p95 = durations.map(values => percentile(values, 0.95));

    // Concurrency sweep: active = started but not ended; queue = queued but not started.
    const activeSeries = Array.from({ length: BUCKETS }, () => 0);
    const queueSeries = Array.from({ length: BUCKETS }, () => 0);
    for (let i = 0; i < BUCKETS; i += 1) {
      const t = start + i * bucketMs + bucketMs / 2;
      for (const run of items) {
        const queued = new Date(run.queued_at).getTime();
        const started = run.started_at ? new Date(run.started_at).getTime() : null;
        const ended = run.ended_at ? new Date(run.ended_at).getTime() : Number.POSITIVE_INFINITY;
        if (queued <= t && (started === null || t < started) && t < ended) queueSeries[i] += 1;
        if (started !== null && started <= t && t < ended) activeSeries[i] += 1;
      }
    }

    const timeLabels = [0, Math.floor(BUCKETS / 3), Math.floor((2 * BUCKETS) / 3), BUCKETS - 1].map(i =>
      new Date(start + i * bucketMs).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
    );
    return { success, failure, p50, p95, activeSeries, queueSeries, timeLabels };
  }, [stats.items, windowMs, now]);

  const workerRows = useMemo(() => {
    const rows = { healthy: 0, attention: 0, stopped: 0 };
    for (const name of workerNames) {
      const state = workerStates[name];
      if (!state) continue;
      if ((state.failures ?? 0) > 0 || state.held) rows.attention += 1;
      else if (state.active) rows.healthy += 1;
      else rows.stopped += 1;
    }
    const total = rows.healthy + rows.attention + rows.stopped;
    return { rows, total };
  }, [workerNames, workerStates]);

  const rangeKeys: RangeKey[] = ['15m', '1h', '24h', '7d'];
  const activeRange = range;

  return (
    <div className="space-y-4">
      <PageHeader
        title="Metrics"
        subtitle={
          <span className="inline-flex items-center gap-1.5">
            Computed from run history · updated live <span className="dot dot-green dot-pulse" />
          </span>
        }
        actions={
          <>
            <div className="tab-seg">
              {rangeKeys.map(key => (
                <button key={key} type="button" className={activeRange === key ? 'active' : ''} onClick={() => setRange(key)}>
                  {key}
                </button>
              ))}
            </div>
            <span className="chip chip-neutral" title="Raw Prometheus metrics are planned for a later release">
              <Icon name="file-text" size={13} />
              REST API stats
            </span>
          </>
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

      {/* Charts row 1 */}
      <div className="grid gap-4 xl:grid-cols-2">
        <ChartPanel
          title="Run duration"
          right={
            <span className="flex items-center gap-3">
              <span className="num text-xs">
                <span className="text-blue-400">p50</span> {stats.median === null ? '—' : formatDurationMs(stats.median)}
                <span className="mx-2 faint">·</span>
                <span className="text-green-400">p95</span> {stats.p95 === null ? '—' : formatDurationMs(stats.p95)}
              </span>
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
              { name: 'p50', color: BLUE, points: charts.p50.map(ms => (ms === null ? null : ms / 1000)) },
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

      {/* Charts row 2 */}
      <div className="grid gap-4 xl:grid-cols-2">
        <ChartPanel
          title="Active runs & queue depth"
          right={
            <span className="num text-xs">
              <span className="text-blue-400">Active {stats.active}</span>
              <span className="mx-2 faint">·</span>
              <span className="text-amber-400">Queue 0</span>
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
          />
        </ChartPanel>

        <section className="panel p-4 sm:p-5">
          <h2 className="panel-title mb-3">Worker states</h2>
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
              <tr>
                <td className="font-medium">Total</td>
                <td className="num text-right">{workerRows.total}</td>
                <td className="num text-right">100%</td>
              </tr>
            </tbody>
          </table>
          {workerRows.total === 0 && <p className="mt-3 text-xs faint">No workers defined.</p>}
        </section>
      </div>

      {/* Reference + API note */}
      <div className="grid gap-4 xl:grid-cols-[1.6fr_1fr]">
        <section className="panel p-4 sm:p-5">
          <h2 className="panel-title mb-3">Run statistics</h2>
          <table className="mc-table">
            <thead>
              <tr>
                <th>Statistic</th>
                <th>Value</th>
                <th>Window</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.queued</td>
                <td className="num">{stats.total}</td>
                <td className="muted">{RANGE_LABEL[activeRange]}</td>
              </tr>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.succeeded</td>
                <td className="num">{stats.succeeded}</td>
                <td className="muted">{RANGE_LABEL[activeRange]}</td>
              </tr>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.failed</td>
                <td className="num">{stats.failed}</td>
                <td className="muted">{RANGE_LABEL[activeRange]}</td>
              </tr>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.duration_p50</td>
                <td className="num">{stats.median === null ? '—' : formatDurationMs(stats.median)}</td>
                <td className="muted">{RANGE_LABEL[activeRange]}</td>
              </tr>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.duration_p95</td>
                <td className="num">{stats.p95 === null ? '—' : formatDurationMs(stats.p95)}</td>
                <td className="muted">{RANGE_LABEL[activeRange]}</td>
              </tr>
              <tr>
                <td className="font-mono text-[0.8rem]">runs.active</td>
                <td className="num">{stats.active}</td>
                <td className="muted">now</td>
              </tr>
            </tbody>
          </table>
        </section>

        <section className="panel flex flex-col gap-2 p-4 sm:p-5">
          <div className="flex items-center gap-2">
            <Icon name="info" size={16} className="text-blue-400" />
            <h2 className="panel-title">API access requires a token</h2>
          </div>
          <p className="text-sm muted">All REST endpoints — including these statistics — are protected. Include the token in the Authorization header:</p>
          <code className="num rounded-lg border border-base-300 bg-base-200 px-3 py-2 text-xs">Authorization: Bearer &lt;token&gt;</code>
          <p className="text-xs faint">
            Prometheus scraping (<code className="font-mono">/metrics</code>) is planned for a later release.
          </p>
        </section>
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

function ChartPanel({ title, right, legend, children }: { title: string; right?: React.ReactNode; legend?: React.ReactNode; children: React.ReactNode }) {
  return (
    <section className="panel p-4 sm:p-5">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h2 className="panel-title">{title}</h2>
        <div className="flex items-center gap-3">
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
