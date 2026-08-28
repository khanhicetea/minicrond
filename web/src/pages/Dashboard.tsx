import { useEffect, useMemo, useState } from 'react';
import { Link, useLocation } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import RunsTable from '../components/RunsTable';
import { daemonQuery, jobsQuery, runsQuery, useWorkerStates } from '../queries';
import { humanizeSchedule, nextFire } from '../lib/cron';
import { formatCountdown, formatIsoLocal, formatSpan } from '../lib/format';
import { useRange } from '../lib/range';
import { jobPath } from '../lib/routes';
import type { Definition, Run } from '../types';

const NEEDS_ATTENTION = new Set(['failed', 'timeout', 'interrupted']);

/** Overview — operational health and active work at a glance. */
export default function Dashboard() {
  const [, navigate] = useLocation();
  const { ms } = useRange();
  const daemon = useQuery(daemonQuery());
  const jobs = useQuery(jobsQuery());
  const runs = useQuery(runsQuery('', 200));
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const definitions: Definition[] = jobs.data ?? [];
  const recentRuns: Run[] = runs.data ?? [];
  const rangeStart = now - ms;
  const inRange = recentRuns.filter(run => new Date(run.queued_at).getTime() >= rangeStart);
  const failedCount = inRange.filter(run => NEEDS_ATTENTION.has(run.status)).length;
  const activeRuns = recentRuns.filter(run => run.status === 'running' || run.status === 'pending');
  const disabledCount = definitions.filter(definition => definition.enabled === false).length;

  const workerNames = useMemo(() => definitions.filter(d => d.kind === 'worker').map(d => d.name), [definitions]);
  const workerStates = useWorkerStates(workerNames);
  const fatalWorkers = workerNames.filter(name => (workerStates[name]?.failures ?? 0) > 0).length;

  const upNext = useMemo(() => {
    return definitions
      .filter(definition => definition.kind === 'job' && definition.enabled !== false && definition.schedule)
      .map(definition => ({ definition, fire: nextFire(definition.schedule!, now, definition.timezone || 'UTC') }))
      .filter((entry): entry is { definition: Definition; fire: number } => entry.fire !== null)
      .sort((a, b) => a.fire - b.fire)
      .slice(0, 5);
  }, [definitions, now]);

  const schedulerTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const daemonOk = !daemon.isError && daemon.data !== undefined;

  return (
    <div className="space-y-5">
      <PageHeader
        title="Overview"
        subtitle="Operational health and active work at a glance."
      />

      {/* Daemon health banner */}
      <section
        className={`panel flex flex-wrap items-center gap-y-3 px-5 py-4 ${daemon.isError ? 'border-red-500/40' : 'border-green-500/25'}`}
        role="status"
      >
        <div className="flex min-w-[13rem] items-center gap-3 pr-4">
          <span className={`flex h-9 w-9 items-center justify-center rounded-full ${daemonOk ? 'bg-green-500/15 text-green-400' : 'bg-red-500/15 text-red-400'}`}>
            <Icon name={daemonOk ? 'check' : 'x'} size={18} strokeWidth={2.5} />
          </span>
          <span className="text-base font-semibold">{daemon.isError ? 'Daemon unreachable' : daemonOk ? 'Daemon healthy' : 'Connecting…'}</span>
        </div>
        <BannerField label="Version" value={daemon.data ? `v${daemon.data.version}` : '—'} />
        <BannerField label="Scheduler timezone" value={schedulerTz} mono />
        <BannerField
          label="Config state"
          value={jobs.isError ? 'Degraded' : daemonOk ? 'Current' : '—'}
          valueClass={jobs.isError ? 'text-amber-400' : 'text-green-400'}
        />
        <BannerField label="Uptime" value={daemon.data ? formatUptimeShort(daemon.data.uptime_s) : '—'} mono />
      </section>

      {/* Alert cards */}
      <div className="grid gap-4 md:grid-cols-3">
        <button type="button" className="alert-card" onClick={() => navigate('/runs?filter=failed')}>
          <span className="flex h-10 w-10 items-center justify-center rounded-full bg-red-500/15 text-red-400">
            <Icon name="alert-circle" size={20} />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block text-2xl font-bold leading-tight">{failedCount}</span>
            <span className="block text-sm font-medium">Failed runs</span>
            <span className="block text-xs muted">In the selected time range</span>
          </span>
          <Icon name="chevron-right" size={18} className="faint shrink-0" />
        </button>
        <button type="button" className="alert-card" onClick={() => navigate('/jobs?filter=attention')}>
          <span className={`flex h-10 w-10 items-center justify-center rounded-full ${fatalWorkers > 0 ? 'bg-red-500/15 text-red-400' : 'bg-green-500/15 text-green-400'}`}>
            <Icon name="octagon-alert" size={20} />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block text-2xl font-bold leading-tight">{fatalWorkers}</span>
            <span className="block text-sm font-medium">Fatal worker</span>
            <span className="block text-xs muted">{fatalWorkers > 0 ? 'Requires immediate attention' : 'All workers healthy'}</span>
          </span>
          <Icon name="chevron-right" size={18} className="faint shrink-0" />
        </button>
        <button type="button" className="alert-card" onClick={() => navigate('/jobs?filter=disabled')}>
          <span className="flex h-10 w-10 items-center justify-center rounded-full bg-[color-mix(in_srgb,var(--color-base-content)_10%,transparent)] faint">
            <Icon name="pause" size={18} />
          </span>
          <span className="min-w-0 flex-1">
            <span className="block text-2xl font-bold leading-tight">{disabledCount}</span>
            <span className="block text-sm font-medium">Disabled jobs</span>
            <span className="block text-xs muted">Not scheduled</span>
          </span>
          <Icon name="chevron-right" size={18} className="faint shrink-0" />
        </button>
      </div>

      {/* Active runs + Up next */}
      <div className="grid gap-4 lg:grid-cols-2">
        <section className="panel flex flex-col p-4 sm:p-5">
          <div className="mb-3 flex items-center justify-between">
            <h2 className="panel-title flex items-center gap-2">
              Active Runs
              {activeRuns.length > 0 && <span className="chip chip-info !py-0">{activeRuns.length}</span>}
            </h2>
            <Link href="/runs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
              View all
            </Link>
          </div>
          {runs.isPending ? (
            <div className="skeleton h-28 w-full" />
          ) : activeRuns.length === 0 ? (
            <div className="flex flex-1 flex-col items-center justify-center py-8 text-center">
              <Icon name="zap" size={20} className="faint" />
              <p className="mt-2 text-sm muted">Nothing is running right now.</p>
            </div>
          ) : (
            <ul className="divide-y divide-[color-mix(in_srgb,var(--color-base-300)_60%,transparent)]">
              {activeRuns.slice(0, 4).map(run => (
                <li key={run.run_id} className="flex items-center gap-3 py-3 first:pt-0 last:pb-0">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2 text-xs">
                      <span className={`dot ${run.status === 'running' ? 'dot-green dot-pulse' : 'dot-blue'}`} />
                      <span className="font-medium capitalize">{run.status}</span>
                    </div>
                    <Link href={`/runs/${run.run_id}`} className="mt-0.5 block truncate font-mono text-[0.9rem] font-semibold hover:text-sky-300">
                      {run.job}
                    </Link>
                    <div className="num faint mt-0.5 truncate">
                      {run.job} · {run.trigger} · started {formatSpan(run.started_at ?? run.queued_at)} ago · {formatIsoLocal(run.started_at ?? run.queued_at)}
                    </div>
                  </div>
                  <Link href={`/runs/${run.run_id}`} className="btn-sub !py-1.5 !px-2.5 !text-xs shrink-0">
                    View log
                    <Icon name="external-link" size={12} />
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </section>

        <section className="panel p-4 sm:p-5">
          <div className="mb-2 flex items-center justify-between">
            <h2 className="panel-title">Up Next</h2>
            <Link href="/jobs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
              View all
            </Link>
          </div>
          {upNext.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-8 text-center">
              <Icon name="clock" size={20} className="faint" />
              <p className="mt-2 text-sm muted">No scheduled firings ahead.</p>
            </div>
          ) : (
            <ul>
              {upNext.map(({ definition, fire }, index) => (
                <li key={definition.name} className="flex items-center gap-3 border-b border-[color-mix(in_srgb,var(--color-base-300)_55%,transparent)] py-2.5 last:border-0">
                  <span className="rank">{index + 1}</span>
                  <Link href={jobPath(definition.name)} className="min-w-0 flex-1 truncate font-mono text-[0.8rem] hover:text-sky-300">
                    {definition.name}
                  </Link>
                  <span className="hidden text-xs muted sm:block">{humanizeSchedule(definition.schedule ?? '')}</span>
                  <span className="num w-[4.5rem] text-right font-medium">{formatCountdown(fire, now)}</span>
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>

      {/* Recent runs */}
      <section>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="panel-title">Recent Runs</h2>
          <Link href="/runs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
            View all runs
          </Link>
        </div>
        {runs.isPending ? <div className="skeleton h-48 w-full" /> : <RunsTable runs={recentRuns} compact />}
      </section>
    </div>
  );
}

function BannerField({ label, value, mono = false, valueClass = '' }: { label: string; value: string; mono?: boolean; valueClass?: string }) {
  return (
    <div className="min-w-[9rem] border-l border-base-300 px-4 py-0.5 first:border-0 sm:border-l">
      <div className="text-xs muted">{label}</div>
      <div className={`mt-0.5 truncate text-sm font-medium ${mono ? 'font-mono text-[0.8rem]' : ''} ${valueClass}`}>{value}</div>
    </div>
  );
}

function formatUptimeShort(seconds: number): string {
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}
