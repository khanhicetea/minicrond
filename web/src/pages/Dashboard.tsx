import { useEffect, useState } from 'react';
import { Link, useLocation } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon, type IconName } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import RunsTable, { COMPACT_LIMIT } from '../components/RunsTable';
import { daemonQuery, jobsQuery, runsQuery, RECENT_RUNS_LIMIT, useWorkerStates } from '../queries';
import { humanizeSchedule } from '../lib/cron';
import { formatClock, formatCountdown, formatDayTime, formatSpan, formatUptime } from '../lib/format';
import { useRange } from '../lib/range';
import { jobPath } from '../lib/routes';
import type { Definition, Run } from '../types';

const NEEDS_ATTENTION = new Set(['failed', 'timeout', 'interrupted']);

/** Trigger badge metadata (daemon emits "schedule" | "manual"). */
const TRIGGER_META: Record<string, { icon: IconName; cls: string }> = {
  schedule: { icon: 'calendar', cls: 'chip-neutral' },
  manual: { icon: 'zap', cls: 'chip-info' },
};

/** Shared tight-badge sizing for the compact run rows. */
const BADGE = '!gap-1 !px-1.5 !py-0 !text-[0.68rem] font-medium';

/** Overview — operational health and active work at a glance. */
export default function Dashboard() {
  const [, navigate] = useLocation();
  const { ms } = useRange();
  const daemon = useQuery(daemonQuery());
  // Keep scheduler-owned next-fire times current as jobs roll over.
  const jobs = useQuery({ ...jobsQuery(), refetchInterval: 1000 });
  const runs = useQuery(runsQuery('', RECENT_RUNS_LIMIT));
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  const definitions: Definition[] = jobs.data ?? [];
  const recentRuns: Run[] = runs.data ?? [];
  const shownRuns = Math.min(recentRuns.length, COMPACT_LIMIT);
  const shownAll = recentRuns.length <= COMPACT_LIMIT;
  const rangeStart = now - ms;
  const inRange = recentRuns.filter(run => new Date(run.queued_at).getTime() >= rangeStart);
  const failedCount = inRange.filter(run => NEEDS_ATTENTION.has(run.status)).length;
  const activeRuns = recentRuns.filter(run => run.status === 'running' || run.status === 'pending');
  const disabledCount = definitions.filter(definition => definition.enabled === false).length;

  const workerNames = definitions.filter(d => d.kind === 'worker').map(d => d.name);
  const workerStates = useWorkerStates(workerNames);
  const fatalWorkers = workerNames.filter(name => (workerStates[name]?.failures ?? 0) > 0).length;

  const upNext = definitions
    .filter(definition => definition.kind === 'job' && definition.enabled !== false && definition.next_fire_at)
    .map(definition => ({ definition, fire: Date.parse(definition.next_fire_at!) }))
    .filter(entry => Number.isFinite(entry.fire))
    .sort((a, b) => a.fire - b.fire)
    .slice(0, 6);

  const schedulerTz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const healthState = daemon.isError ? 'error' : daemon.data ? 'ok' : 'connecting';
  const configDegraded = jobs.isError;
  const healthTitle =
    healthState === 'ok'
      ? `Daemon healthy · v${daemon.data!.version} · up ${formatUptime(daemon.data!.uptime_s)} · ${schedulerTz} · config ${configDegraded ? 'degraded' : 'current'}`
      : healthState === 'error'
        ? 'Daemon unreachable — data may be stale'
        : 'Connecting to daemon…';

  return (
    <div className="space-y-5">
      <PageHeader
        title="Overview"
        subtitle="Operational health and active work at a glance."
        actions={
          <div className="flex flex-wrap items-center gap-2">
            {/* Icon-only daemon health check; hover for details. */}
            <span className={`health-check ${healthState}`} title={healthTitle}>
              <Icon
                name={healthState === 'ok' ? 'check' : healthState === 'error' ? 'x' : 'loader'}
                size={16}
                strokeWidth={2.5}
                className={healthState === 'connecting' ? 'spin' : ''}
              />
              {healthState === 'ok' && <span className="ping" aria-hidden />}
            </span>

            {/* Related health group. */}
            <div className="health-stats">
              <span className="health-stat" title="Daemon version">
                <span className="label">ver</span>
                <span className="val">{daemon.data ? `v${daemon.data.version}` : '—'}</span>
              </span>
              <span className="health-stat" title="Daemon uptime">
                <span className="label">up</span>
                <span className="val">{daemon.data ? formatUptime(daemon.data.uptime_s) : '—'}</span>
              </span>
              <span className="health-stat" title="Scheduler timezone">
                <span className="label">tz</span>
                <span className="val max-w-[9rem] truncate">{schedulerTz}</span>
              </span>
              <span className="health-stat" title="Config state">
                <span className="label">cfg</span>
                <span className={`val ${configDegraded ? 'text-amber-400' : 'text-green-400'}`}>
                  {configDegraded ? 'Degraded' : 'Current'}
                </span>
              </span>
            </div>

            {/* Alert badges — click to jump to the filtered list. */}
            <button
              type="button"
              className={`chip chip-btn !py-2 ${failedCount > 0 ? 'chip-error' : 'chip-neutral'}`}
              title="Failed runs in the selected time range"
              onClick={() => navigate('/runs?filter=failed')}
            >
              <Icon name="alert-circle" size={11} />
              {failedCount} failed
            </button>
            <button
              type="button"
              className={`chip chip-btn !py-2 ${fatalWorkers > 0 ? 'chip-error' : 'chip-neutral'}`}
              title={fatalWorkers > 0 ? 'Workers with fatal failures' : 'All workers healthy'}
              onClick={() => navigate('/jobs?filter=attention')}
            >
              <Icon name="octagon-alert" size={11} />
              {fatalWorkers} fatal
            </button>
            <button
              type="button"
              className="chip chip-btn chip-neutral !py-2"
              title="Disabled jobs (not scheduled)"
              onClick={() => navigate('/jobs?filter=disabled')}
            >
              <Icon name="pause" size={11} />
              {disabledCount} paused
            </button>
          </div>
        }
      />

      {/* 4/8 split: activity rail on the left, recent runs on the right. */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-12">
        <div className="flex min-w-0 flex-col gap-4 lg:col-span-4">
          <ActiveRunsPanel runs={activeRuns} pending={runs.isPending} now={now} />
          <UpNextPanel entries={upNext} now={now} />
        </div>

        <section className="panel flex min-w-0 flex-col self-start lg:col-span-8">
          <div className="flex items-center justify-between border-b border-[color-mix(in_srgb,var(--color-base-300)_60%,transparent)] px-4 py-3">
            <h2 className="panel-title flex items-center gap-2">
              Recent Runs
              {recentRuns.length > 0 && (
                <span
                  className="chip chip-neutral !px-1.5 !py-0 font-mono !text-[0.68rem]"
                  title={shownAll ? `${recentRuns.length} runs` : `Showing ${shownRuns} of ${recentRuns.length} fetched runs`}
                >
                  {shownAll ? recentRuns.length : `${shownRuns}+`}
                </span>
              )}
            </h2>
            <Link href="/runs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
              View all runs
            </Link>
          </div>
          {runs.isPending ? (
            <div className="skeleton m-4 h-48" />
          ) : (
            <RunsTable runs={recentRuns} compact bare />
          )}
        </section>
      </div>
    </div>
  );
}

/* ---------- Active runs (compact) ---------- */

function ActiveRunsPanel({ runs, pending, now }: { runs: Run[]; pending: boolean; now: number }) {
  return (
    <section className="panel flex flex-col p-4">
      <div className="mb-1 flex items-center justify-between">
        <h2 className="panel-title flex items-center gap-2">
          Active Runs
          {runs.length > 0 && (
            <span className="chip chip-info !px-1.5 !py-0 font-mono !text-[0.68rem]">{runs.length}</span>
          )}
        </h2>
        <Link href="/runs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
          View all
        </Link>
      </div>
      {pending ? (
        <div className="skeleton h-24 w-full" />
      ) : runs.length === 0 ? (
        <div className="flex flex-1 flex-col items-center justify-center py-7 text-center">
          <Icon name="zap" size={18} className="faint" />
          <p className="mt-1.5 text-sm muted">Nothing is running right now.</p>
        </div>
      ) : (
        <ul className="divide-y divide-[color-mix(in_srgb,var(--color-base-300)_60%,transparent)]">
          {runs.slice(0, 6).map(run => (
            <ActiveRunRow key={run.run_id} run={run} now={now} />
          ))}
        </ul>
      )}
    </section>
  );
}

function ActiveRunRow({ run, now }: { run: Run; now: number }) {
  const started = run.started_at ?? run.queued_at;
  const running = run.status === 'running';
  const trigger = TRIGGER_META[run.trigger] ?? { icon: 'zap' as IconName, cls: 'chip-neutral' };
  return (
    <li className="py-2">
      <div className="flex items-center gap-2">
        <span className={`dot ${running ? 'dot-green dot-pulse' : 'dot-blue'}`} />
        <Link
          href={`/runs/${run.run_id}`}
          className="min-w-0 flex-1 truncate font-mono text-[0.8rem] font-semibold hover:text-sky-300"
        >
          {run.job}
        </Link>
        <span
          className={`chip !gap-1 !px-1.5 !py-0 font-mono !text-[0.68rem] ${running ? 'chip-info' : 'chip-neutral'}`}
          title={running ? 'Elapsed' : 'Queued for'}
        >
          <Icon name={running ? 'loader' : 'clock'} size={10} className={running ? 'spin' : ''} strokeWidth={2.4} />
          {formatSpan(started, new Date(now).toISOString())}
        </span>
      </div>
      <div className="mt-1.5 flex items-center gap-1 pl-[1.125rem]">
        <span className={`chip ${BADGE} ${running ? 'chip-info' : 'chip-neutral'}`}>
          <Icon name={running ? 'loader' : 'clock'} size={10} className={running ? 'spin' : ''} strokeWidth={2.4} />
          {running ? 'Running' : 'Queued'}
        </span>
        <span className={`chip ${BADGE} ${trigger.cls}`} title={`Trigger: ${run.trigger}`}>
          <Icon name={trigger.icon} size={10} />
          {run.trigger}
        </span>
        {run.attempt > 1 && (
          <span className={`chip ${BADGE} chip-warn`} title={`Attempt ${run.attempt}`}>
            <Icon name="rotate-ccw" size={10} />
            attempt {run.attempt}
          </span>
        )}
        <span className="num faint ml-auto text-[0.68rem]" title="Started at">
          {formatClock(started)}
        </span>
      </div>
    </li>
  );
}

/* ---------- Up next ---------- */

function UpNextPanel({ entries, now }: { entries: { definition: Definition; fire: number }[]; now: number }) {
  return (
    <section className="panel flex flex-col p-4">
      <div className="mb-1 flex items-center justify-between">
        <h2 className="panel-title">Up Next</h2>
        <Link href="/jobs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
          View all
        </Link>
      </div>
      {entries.length === 0 ? (
        <div className="flex flex-col items-center justify-center py-7 text-center">
          <Icon name="clock" size={18} className="faint" />
          <p className="mt-1.5 text-sm muted">No scheduled firings ahead.</p>
        </div>
      ) : (
        <ul>
          {entries.map(({ definition, fire }, index) => (
            <li
              key={definition.name}
              className="flex items-center gap-2 border-b border-[color-mix(in_srgb,var(--color-base-300)_55%,transparent)] py-2 last:border-0"
            >
              <span className="rank">{index + 1}</span>
              <Link href={jobPath(definition.name)} className="min-w-0 flex-1 truncate font-mono text-[0.8rem] hover:text-sky-300">
                {definition.name}
              </Link>
              <span
                className="hidden max-w-[7rem] truncate text-[0.7rem] muted md:block"
                title={definition.schedule}
              >
                {humanizeSchedule(definition.schedule ?? '')}
              </span>
              <div className="flex flex-col items-end gap-0.5">
                <span
                  className={`chip !justify-center !px-1.5 !py-0 font-mono !text-[0.7rem] ${index === 0 ? 'chip-info' : 'chip-neutral'} ${fire > now ? '' : '!text-amber-400'} w-[6rem]`}
                  title={index === 0 ? 'Next firing' : undefined}
                >
                  {fire > now ? `in ${formatCountdown(fire, now)}` : 'Firing…'}
                </span>
                <span className="text-[0.65rem] whitespace-nowrap muted" title="Scheduled fire time">
                  {formatDayTime(new Date(fire).toISOString())}
                </span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
