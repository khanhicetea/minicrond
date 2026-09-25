import { useEffect, useRef, useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { errorText } from '../api';
import { useAuthState } from '../auth';
import { jobsQuery, runsQuery, RECENT_RUNS_LIMIT, useDeleteJob, useSetJobEnabled, useTriggerJob, useWorkerStates } from '../queries';
import { humanizeSchedule } from '../lib/cron';
import { definitionToToml } from '../lib/toml';
import { formatCountdown, formatDayTime, formatSpan } from '../lib/format';
import { jobPath } from '../lib/routes';
import type { Definition, Run } from '../types';

type FilterKey = 'all' | 'jobs' | 'workers' | 'enabled' | 'attention' | 'disabled';
type SortKey = 'name' | 'schedule' | 'next' | 'last';

export default function Jobs() {
  const { mode } = useAuthState();
  const [, navigate] = useLocation();
  const params = new URLSearchParams(useSearch());
  const initialFilter = (params.get('filter') as FilterKey) || 'all';

  const [filter, setFilter] = useState<FilterKey>(initialFilter);
  const [query, setQuery] = useState('');
  const [sortKey, setSortKey] = useState<SortKey>('name');
  const [sortAsc, setSortAsc] = useState(true);
  const [copied, setCopied] = useState(false);
  const [actionError, setActionError] = useState('');
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => setFilter(initialFilter), [initialFilter]);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  // Refresh the server-owned schedule/run state so the countdown never
  // drifts from the scheduler when a fire occurs.
  const jobs = useQuery({ ...jobsQuery(), refetchInterval: 15_000, refetchIntervalInBackground: false });
  const runs = useQuery({ ...runsQuery('', RECENT_RUNS_LIMIT), refetchInterval: 5_000, refetchIntervalInBackground: false });
  const trigger = useTriggerJob();
  const setEnabled = useSetJobEnabled();
  const remove = useDeleteJob();

  const definitions = jobs.data ?? [];
  const workerNames = definitions.filter(d => d.kind === 'worker').map(d => d.name);
  const workerStates = useWorkerStates(workerNames);

  // Latest run per definition.
  const lastRun = new Map<string, Run>();
  for (const run of runs.data ?? []) {
    if (!lastRun.has(run.job)) lastRun.set(run.job, run);
  }

  const workerAttention = (definition: Definition): boolean => {
    const state = workerStates[definition.name];
    return (state?.failures ?? 0) > 0 || state?.held === true;
  };

  const counts = {
    all: definitions.length,
    jobs: definitions.filter(d => d.kind === 'job').length,
    workers: workerNames.length,
    enabled: definitions.filter(d => d.enabled !== false).length,
    attention:
      definitions.filter(
        d =>
          (d.kind === 'worker' && workerAttention(d)) ||
          (d.kind === 'job' && d.enabled !== false && (lastRun.get(d.name)?.status === 'failed' || lastRun.get(d.name)?.status === 'timeout')),
      ).length,
    disabled: definitions.filter(d => d.enabled === false).length,
  };

  const q = query.trim().toLowerCase();
  let filtered = definitions.filter(definition => {
    if (q && !definition.name.toLowerCase().includes(q) && !(definition.labels?.description ?? '').toLowerCase().includes(q)) return false;
    switch (filter) {
      case 'jobs':
        return definition.kind === 'job';
      case 'workers':
        return definition.kind === 'worker';
      case 'enabled':
        return definition.enabled !== false;
      case 'disabled':
        return definition.enabled === false;
      case 'attention':
        return workerAttention(definition) || (definition.kind === 'job' && ['failed', 'timeout'].includes(lastRun.get(definition.name)?.status ?? ''));
      default:
        return true;
    }
  });
  filtered = [...filtered].sort((a, b) => {
      let cmp = 0;
      if (sortKey === 'name') cmp = a.name.localeCompare(b.name);
      if (sortKey === 'schedule') cmp = (a.schedule ?? '').localeCompare(b.schedule ?? '');
      if (sortKey === 'next') {
        const nextOf = (d: Definition) => {
          if (d.kind !== 'job' || d.enabled === false || !d.next_fire_at) return Number.MAX_SAFE_INTEGER;
          const value = Date.parse(d.next_fire_at);
          return Number.isFinite(value) ? value : Number.MAX_SAFE_INTEGER;
        };
        cmp = nextOf(a) - nextOf(b);
      }
      if (sortKey === 'last') {
        const lastOf = (d: Definition) => new Date(lastRun.get(d.name)?.queued_at ?? 0).getTime();
        cmp = lastOf(b) - lastOf(a);
      }
      return sortAsc ? cmp : -cmp;
    });

  const copyCurl = async () => {
    const sample = filtered[0];
    const lines = [
      '# minicron REST API — replace $MINICRON_TOKEN (rotate locally if lost)',
      'BASE=http://127.0.0.1:7423/api/v1',
      '',
      '# List definitions',
      'curl -s -H "Authorization: Bearer $MINICRON_TOKEN" $BASE/jobs',
      '',
      sample ? `# Trigger ${sample.name}\ncurl -s -X POST -H "Authorization: Bearer $MINICRON_TOKEN" $BASE/jobs/${sample.name}/trigger` : '',
      sample ? `\n# Edit ${sample.name} (send the full JSON definition)\ncurl -s -X PUT -H "Authorization: Bearer $MINICRON_TOKEN" -H 'Content-Type: application/json' --data @definition.json $BASE/jobs/${sample.name}` : '',
      sample ? `\n# Delete ${sample.name}\ncurl -s -X DELETE -H "Authorization: Bearer $MINICRON_TOKEN" $BASE/jobs/${sample.name}` : '',
    ];
    try {
      await navigator.clipboard.writeText(lines.filter(Boolean).join('\n'));
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      setActionError('Clipboard unavailable in this browser.');
    }
  };

  const exportOne = (definition: Definition) => {
    const { text } = definitionToToml(definition);
    const blob = new Blob([text], { type: 'application/toml' });
    const link = document.createElement('a');
    link.href = URL.createObjectURL(blob);
    link.download = `${definition.name}.toml`;
    link.click();
    URL.revokeObjectURL(link.href);
  };

  return (
    <div className="space-y-4">
      <PageHeader
        title="Jobs & Workers"
        actions={
          <>
            <Link href="/settings?tab=import" className="btn-sub">
              <Icon name="upload" size={14} />
              Import
            </Link>
            <Link href="/jobs/new" className="btn-primary-x">
              <Icon name="plus" size={14} />
              New definition
            </Link>
          </>
        }
      />

      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex min-w-[13rem] flex-1 items-center gap-2 rounded-lg border border-base-300 bg-base-100 px-3 py-1.5 sm:max-w-xs">
          <Icon name="search" size={14} className="faint shrink-0" />
          <input
            value={query}
            onChange={event => setQuery(event.target.value)}
            placeholder="Search definitions"
            className="w-full bg-transparent text-sm outline-none placeholder:text-[color-mix(in_srgb,var(--color-base-content)_35%,transparent)]"
            aria-label="Search definitions"
          />
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <FilterPill label="All" count={counts.all} active={filter === 'all'} onClick={() => setFilter('all')} />
          <FilterPill label="Jobs" count={counts.jobs} active={filter === 'jobs'} onClick={() => setFilter('jobs')} />
          <FilterPill label="Workers" count={counts.workers} active={filter === 'workers'} onClick={() => setFilter('workers')} />
          <button type="button" className={`pill ${filter === 'enabled' ? 'active' : ''}`} onClick={() => setFilter(filter === 'enabled' ? 'all' : 'enabled')}>
            <span className="dot dot-green" /> Enabled
          </button>
          <button type="button" className={`pill ${filter === 'attention' ? 'active' : ''}`} onClick={() => setFilter(filter === 'attention' ? 'all' : 'attention')}>
            <span className="dot dot-amber" /> Needs attention
          </button>
          {counts.disabled > 0 && <FilterPill label="Disabled" count={counts.disabled} active={filter === 'disabled'} onClick={() => setFilter('disabled')} />}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <label className="flex items-center gap-1.5 text-xs muted">
            Sort by:
            <select className="mc-select !w-auto !py-1 !text-xs" value={sortKey} onChange={event => setSortKey(event.target.value as SortKey)}>
              <option value="name">Name</option>
              <option value="schedule">Schedule</option>
              <option value="next">Next fire</option>
              <option value="last">Last run</option>
            </select>
          </label>
          <button type="button" className="btn-icon border border-base-300" onClick={() => setSortAsc(value => !value)} aria-label="Toggle sort direction">
            <Icon name="arrow-up-down" size={14} />
          </button>
        </div>
      </div>

      {(actionError || trigger.error || remove.error) && (
        <div role="alert" className="rounded-lg border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-300">
          {actionError || errorText(trigger.error ?? remove.error)}
        </div>
      )}

      {(jobs.isError || runs.isError) && (
        <div role="alert" className="rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-300">
          {jobs.isError ? `Definitions could not be loaded: ${errorText(jobs.error)}.` : `Recent run status could not be loaded: ${errorText(runs.error)}.`}
          {jobs.data || runs.data ? ' Displayed data may be stale.' : ''}
          <button type="button" className="ml-3 underline" onClick={() => { void jobs.refetch(); void runs.refetch(); }}>Retry</button>
        </div>
      )}

      {/* Table */}
      <div className="panel overflow-x-auto">
        {jobs.isPending ? (
          <div className="skeleton m-4 h-64" />
        ) : jobs.isError && !jobs.data ? (
          <p className="px-6 py-14 text-center text-sm muted">Definitions are unavailable. Retry the request above.</p>
        ) : filtered.length === 0 ? (
          <div className="px-6 py-14 text-center">
            <Icon name="inbox" size={24} className="mx-auto faint" />
            <p className="mt-2 text-sm muted">
              {definitions.length === 0 ? 'No definitions yet. Create one or add jobs to your TOML config and reload.' : 'No definitions match the current filters.'}
            </p>
            {definitions.length === 0 && (
              <Link href="/jobs/new" className="btn-primary-x mt-4 inline-flex">
                <Icon name="plus" size={14} /> New definition
              </Link>
            )}
          </div>
        ) : (
          <table className="mc-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Schedule / state</th>
                <th>Next fire</th>
                <th>Last run</th>
                <th>Active</th>
                <th>Enabled</th>
                <th className="w-10" aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {filtered.map(definition => (
                <Row
                  key={definition.name}
                  definition={definition}
                  lastRun={lastRun.get(definition.name)}
                  workerState={workerStates[definition.name]}
                  now={now}
                  busy={setEnabled.isPending || trigger.isPending || remove.isPending}
                  onTrigger={() =>
                    trigger.mutate(definition.name, {
                      onSuccess: run => navigate(`/runs/${run.run_id}`),
                      onError: err => setActionError(errorText(err)),
                    })
                  }
                  onToggle={checked => setEnabled.mutate({ name: definition.name, enabled: checked })}
                  onExport={() => exportOne(definition)}
                  onEdit={() => navigate(jobPath(definition.name))}
                  onDuplicate={() => navigate(`/jobs/new?from=${encodeURIComponent(definition.name)}`)}
                  onDelete={() => {
                    if (window.confirm(`Delete definition "${definition.name}"? Past runs and logs are kept until retention cleans them.`)) {
                      remove.mutate(definition.name, { onError: err => setActionError(errorText(err)) });
                    }
                  }}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* curl footer */}
      <div className="panel flex flex-wrap items-center gap-3 px-4 py-3">
        <Icon name="info" size={16} className="faint shrink-0" />
        <p className="min-w-0 flex-1 text-sm muted">
          {mode === 'token' ? 'All actions in this list (trigger, edit, export, delete) can be copied as curl commands.' : 'Use the local minicrond CLI to automate these actions.'}
        </p>
        {mode === 'token' && <button type="button" className="btn-sub" onClick={() => void copyCurl()}>
          <Icon name="terminal" size={14} />
          {copied ? 'Copied!' : 'Copy as curl'}
        </button>}
      </div>
    </div>
  );
}

function FilterPill({ label, count, active, onClick }: { label: string; count: number; active: boolean; onClick: () => void }) {
  return (
    <button type="button" className={`pill ${active ? 'active' : ''}`} onClick={onClick}>
      {label} <span className="count">{count}</span>
    </button>
  );
}

function Row({
  definition,
  lastRun,
  workerState,
  now,
  busy,
  onTrigger,
  onToggle,
  onExport,
  onEdit,
  onDuplicate,
  onDelete,
}: {
  definition: Definition;
  lastRun?: Run;
  workerState?: { held: boolean; active: boolean; failures: number };
  now: number;
  busy: boolean;
  onTrigger: () => void;
  onToggle: (checked: boolean) => void;
  onExport: () => void;
  onEdit: () => void;
  onDuplicate: () => void;
  onDelete: () => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const isWorker = definition.kind === 'worker';
  const enabled = definition.enabled !== false;

  useEffect(() => {
    if (!menuOpen) return;
    const onClick = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) setMenuOpen(false);
    };
    document.addEventListener('mousedown', onClick);
    return () => document.removeEventListener('mousedown', onClick);
  }, [menuOpen]);

  const scheduleLine = isWorker ? 'Continuous' : definition.run_on_start ? 'Startup init' : humanizeSchedule(definition.schedule ?? '') || '—';
  const attention = isWorker ? (workerState?.failures ?? 0) > 0 || workerState?.held === true : ['failed', 'timeout'].includes(lastRun?.status ?? '');
  const stateLine = !enabled
    ? { text: 'Disabled', dot: 'dot-gray' }
    : isWorker
      ? attention
        ? { text: workerState?.held ? 'On hold' : 'Needs attention', dot: 'dot-amber' }
        : workerState?.active
          ? { text: 'Healthy', dot: 'dot-green' }
          : { text: 'Stopped', dot: 'dot-gray' }
      : { text: 'On schedule', dot: 'dot-green' };

  const nextFireMs = !isWorker && enabled && definition.next_fire_at ? Date.parse(definition.next_fire_at) : null;
  const activeCount = lastRun && (lastRun.status === 'running' || lastRun.status === 'pending') ? 1 : 0;

  return (
    <tr>
      <td>
        <div className="flex items-center gap-3">
          <span className={`icon-badge ${isWorker ? 'icon-green' : 'icon-blue'} !h-9 !w-9`}>
            <Icon name={isWorker ? 'terminal' : 'calendar'} size={16} />
          </span>
          <div className="min-w-0">
            <Link href={jobPath(definition.name)} className="block truncate font-mono text-[0.85rem] font-semibold hover:text-sky-300">
              {definition.name}
            </Link>
			{definition.source === 'config' && <span className="text-xs muted">Config owned</span>}
            <div className="truncate text-xs muted">{definition.labels?.description ?? definition.command ?? ((definition.argv ?? []).join(' ') || '—')}</div>
          </div>
        </div>
      </td>
      <td>
        <div className="whitespace-nowrap">{scheduleLine}</div>
        <div className="mt-0.5 flex items-center gap-1.5 text-xs muted">
          <span className={`dot ${stateLine.dot}`} />
          {stateLine.text}
          {isWorker && workerState && !enabled && ' · worker'}
          {isWorker && workerState && (workerState.failures ?? 0) > 0 && <span className="text-amber-400">· {workerState.failures} failures</span>}
        </div>
      </td>
      <td className="whitespace-nowrap">
        {nextFireMs !== null && Number.isFinite(nextFireMs) ? (
          <>
            <div className="num">
              {nextFireMs > now ? `in ${formatCountdown(nextFireMs, now)}` : <span className="text-amber-400">Firing…</span>}
            </div>
            <div className="mt-0.5 text-xs muted">{formatDayTime(new Date(nextFireMs).toISOString())}</div>
          </>
        ) : (
          <span className="faint">—</span>
        )}
      </td>
      <td className="whitespace-nowrap">
        {lastRun ? (
          <>
            <div>
              <Link
                href={`/runs/${lastRun.run_id}`}
                className="text-sky-300 hover:text-sky-200 hover:underline"
                aria-label={`Open last run ${lastRun.run_id}`}
              >
                {lastRun.status === 'running' || lastRun.status === 'pending' ? 'Now' : formatDayTime(lastRun.queued_at)}
              </Link>
            </div>
            <div className="mt-0.5 flex items-center gap-1.5 text-xs">
              {lastRun.status === 'succeeded' ? (
                <span className="inline-flex items-center gap-1 text-green-400">
                  <Icon name="circle-check" size={12} /> {formatSpan(lastRun.started_at, lastRun.ended_at)}
                </span>
              ) : lastRun.status === 'failed' || lastRun.status === 'timeout' ? (
                <span className="inline-flex items-center gap-1 text-red-400">
                  <Icon name="circle-x" size={12} /> {formatSpan(lastRun.started_at, lastRun.ended_at)}
                </span>
              ) : (
                <span className="inline-flex items-center gap-1 text-blue-400 capitalize">
                  <Icon name="loader" size={12} className="spin" /> {lastRun.status}
                </span>
              )}
            </div>
          </>
        ) : (
          <span className="faint">Never</span>
        )}
      </td>
      <td>
        {isWorker ? (
          workerState ? (
            <div>
              <span className="num">{workerState.active ? '1 / 1' : '0 / 1'}</span>
              <div className={`mt-0.5 text-xs ${workerState.failures > 0 ? 'text-red-400' : workerState.active ? 'text-green-400' : 'muted'}`}>
                {workerState.failures > 0 ? `${workerState.failures} fatal` : workerState.active ? 'Healthy' : 'Stopped'}
              </div>
            </div>
          ) : (
            <span className="faint">—</span>
          )
        ) : activeCount ? (
          <span className="chip chip-info !py-0.5">{activeCount}</span>
        ) : (
          <span className="num faint">0</span>
        )}
      </td>
      <td>
        <label className="switch">
          <input
            type="checkbox"
            checked={enabled}
            disabled={busy || definition.source === 'config'}
            onChange={event => onToggle(event.target.checked)}
            aria-label={`Enable ${definition.name}`}
          />
          <span className="track" />
        </label>
      </td>
      <td className="relative text-right">
        <div ref={menuRef} className="inline-block">
          <button type="button" className="btn-icon" onClick={() => setMenuOpen(open => !open)} aria-label={`Actions for ${definition.name}`} aria-expanded={menuOpen}>
            <Icon name="more-vertical" size={16} />
          </button>
          {menuOpen && (
            <div className="menu-pop right-0 top-full mt-1">
              {!isWorker && definition.source !== 'config' && (
                <button
                  type="button"
                  className="menu-item"
                  disabled={!enabled || busy}
                  onClick={() => {
                    setMenuOpen(false);
                    onTrigger();
                  }}
                >
                  <Icon name="play" size={14} /> Trigger now
                </button>
              )}
              {definition.source !== 'config' && <button
                type="button"
                className="menu-item"
                onClick={() => {
                  setMenuOpen(false);
                  onEdit();
                }}
              >
                <Icon name="pencil" size={14} /> Edit
              </button>}
              <button
                type="button"
                className="menu-item"
                onClick={() => {
                  setMenuOpen(false);
                  onExport();
                }}
              >
                <Icon name="download" size={14} /> Export
              </button>
              <button
                type="button"
                className="menu-item"
                onClick={() => {
                  setMenuOpen(false);
                  onDuplicate();
                }}
              >
                <Icon name="copy" size={14} /> Duplicate
              </button>
              {definition.source !== 'config' && <button
                type="button"
                className="menu-item danger"
                disabled={busy}
                onClick={() => {
                  setMenuOpen(false);
                  onDelete();
                }}
              >
                <Icon name="trash" size={14} /> Delete
              </button>}
            </div>
          )}
        </div>
      </td>
    </tr>
  );
}
