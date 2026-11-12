import { useEffect, useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { errorText } from '../api';
import { useAuthState } from '../auth';
import { useDeleteJob, useSetJobEnabled, useTriggerJob, useWorkerAction } from '../queries';
import { humanizeSchedule } from '../lib/cron';
import { definitionToToml } from '../lib/toml';
import { formatCountdown, formatDayTime, formatSpan } from '../lib/format';
import { jobPath } from '../lib/routes';
import { publicURL } from '../lib/base';
import { useOverview, workerNeedsAttention, type AttentionItem } from '../lib/attention';
import { workerAttentionLabel, workerStatusLabel } from '../lib/worker-state';
import {
  ActionMenu,
  Badge,
  Button,
  Callout,
  EmptyState,
  InlineAlert,
  PageHeader,
  RunStrip,
  SearchField,
  Segmented,
  StatusLabel,
  buttonClass,
  failedCount,
  isFailedStatus,
  runStatusMeta,
  type ActionItem,
  type Tone,
} from '../components/ui';
import { isActiveRun, type Definition, type Run, type WorkerState } from '../types';

type FilterKey = 'all' | 'attention' | 'jobs' | 'workers' | 'enabled' | 'disabled';
type SortKey = 'attention' | 'next' | 'last' | 'name';
const FILTER_KEYS: FilterKey[] = ['all', 'attention', 'jobs', 'workers', 'enabled', 'disabled'];

interface RowState {
  tone: Tone;
  label: string;
  attention: boolean;
}

function rowState(definition: Definition, lastRun: Run | undefined, worker: WorkerState | undefined): RowState {
  if (definition.enabled === false) return { tone: 'neutral', label: 'Disabled', attention: false };
  if (definition.kind === 'worker') {
    if (!worker) return { tone: 'neutral', label: '—', attention: false };
    if (worker.status === 'exhausted') return { tone: 'danger', label: workerStatusLabel(worker), attention: true };
    if (workerNeedsAttention(worker)) return { tone: 'warn', label: workerAttentionLabel(worker), attention: true };
    if (worker.active) return { tone: 'ok', label: 'Healthy', attention: false };
    return { tone: 'neutral', label: workerStatusLabel(worker), attention: false };
  }
  if (lastRun && isActiveRun(lastRun)) return { tone: 'info', label: runStatusMeta(lastRun.status).label, attention: false };
  if (lastRun && isFailedStatus(lastRun.status)) return { tone: 'danger', label: runStatusMeta(lastRun.status).label, attention: true };
  return { tone: 'ok', label: definition.run_on_start ? 'Startup init' : 'On schedule', attention: false };
}

function scheduleText(definition: Definition): { main: string; sub: string } {
  if (definition.kind === 'worker') return { main: 'Continuous', sub: 'worker' };
  if (definition.run_on_start) return { main: 'At startup', sub: 'run_on_start' };
  const raw = definition.schedule ?? '';
  return { main: humanizeSchedule(raw) || raw || '—', sub: raw };
}

function runDuration(run: Run): string {
  return isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at);
}

export default function Jobs() {
  const { mode } = useAuthState();
  const [, navigate] = useLocation();
  const params = new URLSearchParams(useSearch());
  const requested = params.get('filter') as FilterKey | null;
  const initialFilter: FilterKey = requested && FILTER_KEYS.includes(requested) ? requested : 'all';

  const [filter, setFilter] = useState<FilterKey>(initialFilter);
  const [query, setQuery] = useState('');
  const [sortKey, setSortKey] = useState<SortKey>('attention');
  const [copied, setCopied] = useState(false);
  const [actionError, setActionError] = useState('');
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => setFilter(initialFilter), [initialFilter]);
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);

  // Refresh server-owned schedule/run state so countdowns never drift from the scheduler.
  const { jobs, runs, definitions, runsByJob, workerStates, attention } = useOverview({ poll: true });
  const trigger = useTriggerJob();
  const setEnabled = useSetJobEnabled();
  const remove = useDeleteJob();
  const workerAction = useWorkerAction();
  const busy = setEnabled.isPending || trigger.isPending || remove.isPending || workerAction.isPending;

  const states = new Map(definitions.map(definition => [definition.name, rowState(definition, runsByJob.get(definition.name)?.[0], workerStates[definition.name])]));
  const running = (runs.data ?? []).filter(isActiveRun).length;

  const counts = {
    all: definitions.length,
    attention: attention.length,
    jobs: definitions.filter(d => d.kind === 'job').length,
    workers: definitions.filter(d => d.kind === 'worker').length,
    disabled: definitions.filter(d => d.enabled === false).length,
  };

  const q = query.trim().toLowerCase();
  const filtered = definitions.filter(definition => {
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
        return states.get(definition.name)?.attention === true;
      default:
        return true;
    }
  });

  const nextOf = (d: Definition) => {
    if (d.kind !== 'job' || d.enabled === false || !d.next_fire_at) return Number.MAX_SAFE_INTEGER;
    const value = Date.parse(d.next_fire_at);
    return Number.isFinite(value) ? value : Number.MAX_SAFE_INTEGER;
  };
  const lastOf = (d: Definition) => new Date(runsByJob.get(d.name)?.[0]?.queued_at ?? 0).getTime();
  const rank = (d: Definition) => {
    const state = states.get(d.name);
    return state?.attention ? 0 : state?.tone === 'info' ? 1 : d.enabled === false ? 3 : 2;
  };
  const sorted = [...filtered].sort((a, b) => {
    if (sortKey === 'next') return nextOf(a) - nextOf(b) || a.name.localeCompare(b.name);
    if (sortKey === 'last') return lastOf(b) - lastOf(a) || a.name.localeCompare(b.name);
    if (sortKey === 'attention') return rank(a) - rank(b) || nextOf(a) - nextOf(b) || a.name.localeCompare(b.name);
    return a.name.localeCompare(b.name);
  });

  const nextJob = definitions.filter(d => nextOf(d) !== Number.MAX_SAFE_INTEGER).sort((a, b) => nextOf(a) - nextOf(b))[0];

  const copyCurl = async () => {
    const sample = sorted[0];
    const lines = [
      '# minicrond REST API — replace $MINICROND_TOKEN (rotate locally if lost)',
      `BASE=${publicURL('/api/v1')}`,
      '',
      '# List definitions',
      'curl -s -H "Authorization: Bearer $MINICROND_TOKEN" $BASE/jobs',
      '',
      sample ? `# Trigger ${sample.name}\ncurl -s -X POST -H "Authorization: Bearer $MINICROND_TOKEN" $BASE/jobs/${sample.name}/trigger` : '',
      sample ? `\n# Edit ${sample.name} (send the full JSON definition)\ncurl -s -X PUT -H "Authorization: Bearer $MINICROND_TOKEN" -H 'Content-Type: application/json' --data @definition.json $BASE/jobs/${sample.name}` : '',
      sample ? `\n# Delete ${sample.name}\ncurl -s -X DELETE -H "Authorization: Bearer $MINICROND_TOKEN" $BASE/jobs/${sample.name}` : '',
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
    link.download = 'minicron-definition.toml';
    link.click();
    URL.revokeObjectURL(link.href);
  };

  const runNow = (name: string) => {
    setActionError('');
    trigger.mutate(name, {
      onSuccess: run => navigate(`/runs/${encodeURIComponent(run.run_id)}`),
      onError: err => setActionError(errorText(err)),
    });
  };

  const controlWorker = (name: string, action: 'start' | 'stop' | 'restart') => {
    setActionError('');
    workerAction.mutate({ name, action }, { onError: err => setActionError(errorText(err)) });
  };

  const menuFor = (definition: Definition): ActionItem[] => {
    const configOwned = definition.source === 'config';
    const enabled = definition.enabled !== false;
    const worker = workerStates[definition.name];
    const isWorker = definition.kind === 'worker';
    return [
      { label: 'Run now', icon: 'play', primary: true, hidden: isWorker || configOwned, disabled: !enabled || busy, onSelect: () => runNow(definition.name) },
      { label: 'Restart worker', icon: 'rotate-ccw', primary: true, hidden: !isWorker || configOwned, disabled: !enabled || busy, onSelect: () => controlWorker(definition.name, 'restart') },
      { label: worker?.active ? 'Stop worker' : 'Start worker', icon: worker?.active ? 'square' : 'play', hidden: !isWorker || configOwned, disabled: !enabled || busy, onSelect: () => controlWorker(definition.name, worker?.active ? 'stop' : 'start') },
      { label: enabled ? 'Disable' : 'Enable', checked: enabled, hidden: configOwned, disabled: busy, onSelect: () => setEnabled.mutate({ name: definition.name, enabled: !enabled }, { onError: err => setActionError(errorText(err)) }) },
      { label: configOwned ? 'View definition' : 'Edit definition', icon: configOwned ? 'eye' : 'pencil', separatorBefore: !configOwned, onSelect: () => navigate(jobPath(definition.name)) },
      { label: 'Run history', icon: 'history', onSelect: () => navigate(`/runs?job=${encodeURIComponent(definition.name)}`) },
      { label: 'Clone', icon: 'copy', onSelect: () => navigate(`/jobs/new?from=${encodeURIComponent(definition.name)}`) },
      { label: 'Export TOML', icon: 'download', onSelect: () => exportOne(definition) },
      {
        label: 'Delete…',
        icon: 'trash',
        danger: true,
        separatorBefore: true,
        hidden: configOwned,
        disabled: busy,
        onSelect: () => {
          if (window.confirm(`Delete definition "${definition.name}"? Past runs and logs are kept until retention cleans them.`)) {
            remove.mutate(definition.name, { onError: err => setActionError(errorText(err)) });
          }
        },
      },
    ];
  };

  const subtitle = jobs.isSuccess ? (
    <>
      {counts.jobs} {counts.jobs === 1 ? 'job' : 'jobs'} and {counts.workers} {counts.workers === 1 ? 'worker' : 'workers'}
      {running > 0 && ` · ${running} running now`}
      {nextJob && (
        <>
          {' · next fire '}
          <strong className="font-semibold text-fg">
            {nextJob.name} in {formatCountdown(nextOf(nextJob), now)}
          </strong>
        </>
      )}
    </>
  ) : undefined;

  return (
    <div className="space-y-5">
      <PageHeader
        title="Jobs & workers"
        subtitle={subtitle}
        actions={
          <>
            <Link href="/settings?tab=import" className={buttonClass('secondary')}>
              Import TOML
            </Link>
            <Link href="/jobs/new" className={buttonClass('primary')}>
              New definition
            </Link>
          </>
        }
      />

      {attention.length > 0 && (
        <Callout tone="danger" title={`${attention.length} need${attention.length === 1 ? 's' : ''} attention`} meta={attention.length < definitions.length ? 'Everything else is on schedule.' : undefined}>
          <div className="attention-grid">
            {attention.map(item => (
              <AttentionRow key={item.definition.name} item={item} busy={busy} onRunNow={runNow} onRestart={name => controlWorker(name, 'restart')} />
            ))}
          </div>
        </Callout>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <Segmented<FilterKey>
          label="Filter definitions"
          value={filter}
          onChange={setFilter}
          options={[
            { value: 'all', label: 'All', count: counts.all },
            { value: 'attention', label: 'Needs attention', count: counts.attention, hidden: counts.attention === 0 && filter !== 'attention' },
            { value: 'jobs', label: 'Jobs', count: counts.jobs },
            { value: 'workers', label: 'Workers', count: counts.workers },
            { value: 'disabled', label: 'Disabled', count: counts.disabled, hidden: counts.disabled === 0 && filter !== 'disabled' },
          ]}
        />
        <SearchField value={query} onChange={setQuery} label="Search definitions" className="min-w-[12rem] flex-[0_1_16rem]" />
        <label className="ml-auto flex items-center gap-2 text-sm muted">
          Sort
          <select className="mc-select !w-auto" value={sortKey} onChange={event => setSortKey(event.target.value as SortKey)}>
            <option value="attention">Needs attention first</option>
            <option value="next">Next fire</option>
            <option value="last">Last run</option>
            <option value="name">Name</option>
          </select>
        </label>
      </div>

      {(actionError || trigger.error || remove.error || setEnabled.error || workerAction.error) && (
        <InlineAlert>{actionError || errorText(trigger.error ?? remove.error ?? setEnabled.error ?? workerAction.error)}</InlineAlert>
      )}
      {(jobs.isError || runs.isError) && (
        <InlineAlert
          tone="warn"
          action={
            <Button
              size="sm"
              onClick={() => {
                void jobs.refetch();
                void runs.refetch();
              }}
            >
              Retry
            </Button>
          }
        >
          {jobs.isError ? `Definitions could not be loaded: ${errorText(jobs.error)}.` : `Recent run status could not be loaded: ${errorText(runs.error)}.`}
          {jobs.data || runs.data ? ' Displayed data may be stale.' : ''}
        </InlineAlert>
      )}

      <div className="table-wrap">
        {jobs.isPending ? (
          <div className="skeleton m-4 h-64" />
        ) : jobs.isError && !jobs.data ? (
          <EmptyState icon="alert-circle" title="Definitions are unavailable.">
            Retry the request above.
          </EmptyState>
        ) : sorted.length === 0 ? (
          <EmptyState title={definitions.length === 0 ? 'No definitions yet.' : 'No definitions match the current filters.'}>
            {definitions.length === 0 && (
              <>
                <p>Create one, or add jobs to your TOML config and reload.</p>
                <Link href="/jobs/new" className={buttonClass('primary', 'md', 'mt-4')}>
                  New definition
                </Link>
              </>
            )}
          </EmptyState>
        ) : (
          <table className="mc-table min-w-[52rem]">
            <thead>
              <tr>
                <th scope="col">Definition</th>
                <th scope="col">Status · last 20 runs</th>
                <th scope="col">Schedule</th>
                <th scope="col">Last run</th>
                <th scope="col">Next</th>
                <th scope="col" className="text-right">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {sorted.map(definition => (
                <Row
                  key={definition.name}
                  definition={definition}
                  runs={runsByJob.get(definition.name) ?? []}
                  worker={workerStates[definition.name]}
                  state={states.get(definition.name)!}
                  now={now}
                  actions={menuFor(definition)}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>

      <footer className="flex flex-wrap items-center gap-3 text-sm faint">
        <span>
          Showing {sorted.length} of {definitions.length} definitions
        </span>
        <span className="ml-auto inline-flex flex-wrap items-center gap-3 text-xs">
          <Legend className="s-ok" label="succeeded" />
          <Legend className="s-fail" label="failed / timeout" />
          <Legend className="s-run" label="running" />
        </span>
        {mode === 'token' ? (
          <Button size="sm" icon="terminal" onClick={() => void copyCurl()}>
            {copied ? 'Copied!' : 'Copy as curl'}
          </Button>
        ) : (
          <span className="text-xs">Use the local minicrond CLI to automate these actions.</span>
        )}
      </footer>
    </div>
  );
}

function Legend({ className, label }: { className: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className="run-strip" aria-hidden="true">
        <span className={className} style={{ height: 8 }} />
      </span>
      {label}
    </span>
  );
}

function AttentionRow({ item, busy, onRunNow, onRestart }: { item: AttentionItem; busy: boolean; onRunNow: (name: string) => void; onRestart: (name: string) => void }) {
  const { definition, run, worker } = item;
  const configOwned = definition.source === 'config';
  let badge = 'Needs attention';
  let detail = '';
  let logRun = '';
  if (run) {
    badge = runStatusMeta(run.status).label;
    detail = [run.exit_code !== undefined ? `Exit ${run.exit_code}` : '', `after ${runDuration(run)}`, run.attempt > 1 ? `attempt ${run.attempt}` : '', formatDayTime(run.ended_at ?? run.queued_at)].filter(Boolean).join(' · ');
    logRun = run.run_id;
  } else if (worker) {
    badge = workerAttentionLabel(worker);
    detail = worker.exhaustion?.reason ?? (worker.failures > 0 ? `${worker.failures} recent ${worker.failures === 1 ? 'failure' : 'failures'}` : 'Held by the restart policy');
    logRun = worker.exhaustion?.last_run_id ?? '';
  }
  return (
    <div className="attention-item">
      <div className="min-w-0 flex-[1_1_14rem]">
        <div className="flex flex-wrap items-center gap-2">
          <Link href={jobPath(definition.name)} className="mono font-medium text-fg hover:text-link">
            {definition.name}
          </Link>
          <Badge tone={worker && !run ? 'warn' : 'danger'}>{badge}</Badge>
        </div>
        <div className="attention-detail">{detail}</div>
      </div>
      <div className="flex gap-2">
        {logRun ? (
          <Link href={`/runs/${encodeURIComponent(logRun)}`} className={buttonClass('secondary', 'sm')}>
            View log
          </Link>
        ) : (
          <Link href={`/runs?job=${encodeURIComponent(definition.name)}`} className={buttonClass('secondary', 'sm')}>
            View runs
          </Link>
        )}
        {!configOwned && run && (
          <Button size="sm" variant="danger" icon="play" disabled={busy} onClick={() => onRunNow(definition.name)}>
            Run again
          </Button>
        )}
        {!configOwned && worker && !run && (
          <Button size="sm" variant="danger" icon="rotate-ccw" disabled={busy} onClick={() => onRestart(definition.name)}>
            Restart
          </Button>
        )}
      </div>
    </div>
  );
}

function Row({ definition, runs, worker, state, now, actions }: { definition: Definition; runs: Run[]; worker?: WorkerState; state: RowState; now: number; actions: ActionItem[] }) {
  const isWorker = definition.kind === 'worker';
  const lastRun = runs[0];
  const schedule = scheduleText(definition);
  const failures = failedCount(runs);
  const nextFireMs = !isWorker && definition.enabled !== false && definition.next_fire_at ? Date.parse(definition.next_fire_at) : null;

  return (
    <tr className={definition.enabled === false ? 'row-disabled' : state.attention ? 'row-attention' : ''}>
      <td>
        <div className="flex items-center gap-3">
          <span className={`icon-badge ${isWorker ? 'icon-green' : 'icon-blue'} !h-[1.875rem] !w-[1.875rem]`} title={isWorker ? 'Worker' : 'Job'}>
            {isWorker ? 'W' : 'J'}
            <span className="sr-only">{isWorker ? ' worker' : ' job'}</span>
          </span>
          <div className="min-w-0">
            <Link href={jobPath(definition.name)} className="block truncate mono text-[0.84rem] font-medium text-fg-strong hover:text-link">
              {definition.name}
            </Link>
            <div className="max-w-[18rem] truncate mono text-xs faint" title={definition.labels?.description ?? definition.command ?? (definition.argv ?? []).join(' ')}>
              {definition.source === 'config' && <span className="mr-1.5 font-sans">Config owned ·</span>}
              {definition.labels?.description ?? definition.command ?? ((definition.argv ?? []).join(' ') || '—')}
            </div>
          </div>
        </div>
      </td>
      <td className="whitespace-nowrap">
        <div className="inline-flex flex-col gap-1.5">
          <StatusLabel tone={state.tone}>{state.label}</StatusLabel>
          {isWorker ? (
            <span className="text-xs faint">
              {worker?.active ? '1 / 1 active' : '0 / 1 active'}
              {(worker?.failures ?? 0) > 0 && <span className="text-warn-fg"> · {worker?.failures} {worker?.failures === 1 ? 'failure' : 'failures'}</span>}
            </span>
          ) : (
            <span className="flex items-center gap-2">
              <RunStrip runs={runs} />
              {failures > 0 && <span className="text-[0.72rem] text-danger-fg tabular">{failures} failed</span>}
            </span>
          )}
        </div>
      </td>
      <td>
        <div className="text-sm">{schedule.main}</div>
        <div className="cell-sub mono">{schedule.sub}</div>
      </td>
      <td className="whitespace-nowrap">
        {lastRun ? (
          <>
            <Link href={`/runs/${encodeURIComponent(lastRun.run_id)}`} className="text-sm text-fg hover:text-link" aria-label={`Open last run ${lastRun.run_id}`}>
              {isActiveRun(lastRun) ? 'Running now' : runStatusMeta(lastRun.status).label}
              {lastRun.exit_code !== undefined && lastRun.exit_code !== 0 && ` · exit ${lastRun.exit_code}`}
            </Link>
            <div className="cell-sub">
              {formatDayTime(lastRun.started_at ?? lastRun.queued_at)} · {runDuration(lastRun)}
            </div>
          </>
        ) : (
          <span className="faint">{isWorker ? '—' : 'No recent runs'}</span>
        )}
      </td>
      <td className="whitespace-nowrap tabular">
        {nextFireMs !== null && Number.isFinite(nextFireMs) ? (
          <>
            <div className="text-sm">{nextFireMs > now ? `in ${formatCountdown(nextFireMs, now)}` : <span className="text-warn-fg">Firing…</span>}</div>
            <div className="cell-sub">{formatDayTime(new Date(nextFireMs).toISOString())}</div>
          </>
        ) : (
          <span className="faint">—</span>
        )}
      </td>
      <td className="text-right">
        <ActionMenu label={`Actions for ${definition.name}`} items={actions} />
      </td>
    </tr>
  );
}
