import { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useSearch } from 'wouter';
import { Icon, type IconName } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { api, errorText } from '../api';
import { auth } from '../auth';
import { daemonQuery, jobsQuery, runsQuery, RECENT_RUNS_LIMIT, useReloadDaemon } from '../queries';
import { downloadFile } from '../lib/download';
import { formatDayTime, formatSpan, formatUptime, shortId, shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';
import type { Definition } from '../types';

/** Tight badge sizing shared across settings panels (overview style). */
const BADGE = '!gap-1 !px-1.5 !py-0 !text-[0.68rem] font-medium';

interface Check {
  name: string;
  ok: boolean;
  detail: string;
}

/** Shared daemon/jobs/runs state + diagnostics checks, used by header and panels. */
function useDiagnostics() {
  const daemon = useQuery(daemonQuery());
  const jobs = useQuery(jobsQuery());
  const runs = useQuery(runsQuery('', RECENT_RUNS_LIMIT));
  const definitions = jobs.data ?? [];
  const checks: Check[] = [
    { name: 'Daemon API', ok: !daemon.isError && daemon.data !== undefined, detail: daemon.isError ? errorText(daemon.error) : `v${daemon.data?.version ?? ''}` },
    { name: 'Definitions readable', ok: !jobs.isError && jobs.data !== undefined, detail: jobs.isError ? errorText(jobs.error) : `${definitions.length} loaded` },
    { name: 'Run history readable', ok: !runs.isError && runs.data !== undefined, detail: runs.isError ? errorText(runs.error) : `${runs.data?.length ?? 0} recent runs` },
    { name: 'Log streaming', ok: !runs.isError, detail: runs.isError ? 'unavailable' : 'SSE ready' },
  ];
  return { daemon, jobs, runs, definitions, checks };
}

/** Settings & diagnostics + Export / Import tabs. */
export default function Settings() {
  const initialTab = new URLSearchParams(useSearch()).get('tab') === 'import' ? 'import' : 'main';
  const [tab, setTab] = useState<'main' | 'import'>(initialTab);
  useEffect(() => setTab(initialTab), [initialTab]);

  const { daemon, definitions, checks } = useDiagnostics();
  const failedChecks = checks.filter(check => !check.ok).length;

  const healthState = daemon.isError ? 'error' : daemon.data ? 'ok' : 'connecting';
  const healthTitle =
    healthState === 'ok'
      ? `Daemon healthy · v${daemon.data!.version} · schema v${daemon.data!.schema_version} · up ${formatUptime(daemon.data!.uptime_s)}`
      : healthState === 'error'
        ? 'Daemon unreachable — data may be stale'
        : 'Connecting to daemon…';

  return (
    <div className="space-y-5">
      <PageHeader
        title="Settings & diagnostics"
        subtitle="Security, storage, definition registry, and daemon diagnostics."
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
              <span className="health-stat" title="Schema version">
                <span className="label">schema</span>
                <span className="val">v{daemon.data?.schema_version ?? '—'}</span>
              </span>
              <span className="health-stat" title="Daemon uptime">
                <span className="label">up</span>
                <span className="val">{daemon.data ? formatUptime(daemon.data.uptime_s) : '—'}</span>
              </span>
              <span className="health-stat" title={daemon.data ? `Token fingerprint ${daemon.data.token_fingerprint}` : 'Token fingerprint'}>
                <span className="label">fp</span>
                <span className="val">{daemon.data ? shortId(daemon.data.token_fingerprint, 10) : '—'}</span>
              </span>
            </div>

            {/* Summary badges. */}
            <Link href="/jobs" className="chip chip-btn chip-neutral !py-2" title="All job & worker definitions">
              <Icon name="list" size={11} />
              {definitions.length} defs
            </Link>
            <span
              className={`chip !py-2 ${failedChecks > 0 ? 'chip-error' : 'chip-success'}`}
              title={failedChecks > 0 ? `${failedChecks} of ${checks.length} diagnostics checks failing` : `All ${checks.length} diagnostics checks passing`}
            >
              <Icon name={failedChecks > 0 ? 'alert-circle' : 'circle-check'} size={11} />
              {failedChecks > 0 ? `${failedChecks} failing` : 'all passing'}
            </span>
          </div>
        }
      />
      <div className="border-b border-base-300">
        <button type="button" className={`tab-line ${tab === 'main' ? 'active' : ''}`} onClick={() => setTab('main')}>
          Settings &amp; diagnostics
        </button>
        <button type="button" className={`tab-line ${tab === 'import' ? 'active' : ''}`} onClick={() => setTab('import')}>
          Export / Import
        </button>
      </div>
      {tab === 'main' ? <MainTab /> : <ImportTab />}
    </div>
  );
}

/** Panel with a bordered header row: icon + title + badge, optional right actions. */
function Panel({
  icon,
  title,
  badge,
  actions,
  className = '',
  children,
}: {
  icon: IconName;
  title: string;
  badge?: React.ReactNode;
  actions?: React.ReactNode;
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <section className={`panel flex min-w-0 flex-col ${className}`}>
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[color-mix(in_srgb,var(--color-base-300)_60%,transparent)] px-4 py-3">
        <h2 className="panel-title flex items-center gap-2">
          <Icon name={icon} size={14} className="text-blue-400" />
          {title}
          {badge}
        </h2>
        {actions}
      </div>
      <div className="flex-1 p-4">{children}</div>
    </section>
  );
}

/** Compact icon-only status chip (table cells). */
function StatusChip({ ok, detail }: { ok: boolean; detail?: string }) {
  const label = ok ? 'Healthy' : 'Error';
  return (
    <span className={`chip !gap-1 !px-1.5 !py-0 ${ok ? 'chip-success' : 'chip-error'}`} title={detail ?? label} aria-label={label}>
      <Icon name={ok ? 'circle-check' : 'alert-circle'} size={11} />
    </span>
  );
}

/** One key/value row of the storage list (fits the narrow left column). */
function StorageRow({
  label,
  value,
  ok,
  detail,
  title,
  mono = false,
}: {
  label: string;
  value: string;
  ok?: boolean;
  detail?: string;
  title?: string;
  mono?: boolean;
}) {
  return (
    <li className="flex items-center gap-2 py-2" title={title}>
      <span className="shrink-0 text-sm muted">{label}</span>
      <span className={`ml-auto truncate text-[0.8rem] ${mono ? 'num' : ''}`}>{value}</span>
      {ok !== undefined && <StatusChip ok={ok} detail={detail} />}
    </li>
  );
}

function MainTab() {
  const { daemon, runs, definitions, checks } = useDiagnostics();
  const reload = useReloadDaemon();
  const client = useQueryClient();
  const [copied, setCopied] = useState(false);
  const [reveal, setReveal] = useState(false);
  const [rotateError, setRotateError] = useState('');
  const [liveTail, setLiveTail] = useState(true);

  const failedChecks = checks.filter(check => !check.ok).length;

  const copyToken = async () => {
    try {
      await navigator.clipboard.writeText(auth.token);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // clipboard unavailable
    }
  };

  const rotate = async () => {
    setRotateError('');
    try {
      const result = await api.rotateToken();
      // Only reachable over the local Unix socket; adopt the new token.
      auth.login(result.token);
      void client.invalidateQueries();
    } catch (error) {
      setRotateError(
        error instanceof Error && error.message.includes('local')
          ? 'Token rotation requires the local Unix socket. Run `minicrond token --rotate` on the host.'
          : errorText(error),
      );
    }
  };

  const activity = (runs.data ?? []).slice(0, 14);

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-12">
      {/* Left rail (4): storage, security, diagnostics. */}
      <div className="flex min-w-0 flex-col gap-4 lg:col-span-4">
        {/* Storage & retention */}
        <Panel icon="hard-drive" title="Storage & retention">
          <ul className="divide-y divide-[color-mix(in_srgb,var(--color-base-300)_55%,transparent)]">
            <StorageRow label="Database" value="SQLite (WAL)" ok={!daemon.isError} detail={daemon.isError ? errorText(daemon.error) : 'API reachable'} />
            <StorageRow label="Logs" value="Local filesystem" ok={!runs.isError} detail={runs.isError ? 'unavailable' : 'readable'} />
            <StorageRow label="Schema" value={`v${daemon.data?.schema_version ?? '—'}`} mono ok={!daemon.isError} />
            <StorageRow label="Retention" value="per definition" title="keep_runs / keep_for" />
          </ul>
          <p className="field-help mt-2">Health reflects API reachability from this session.</p>
        </Panel>

        {/* Security */}
        <Panel icon="key" title="Security" badge={<span className={`chip chip-info ${BADGE}`}>bearer</span>}>
          <label className="field-label" htmlFor="admin-token">Admin token</label>
          <div className="flex items-center gap-2">
            <div className="relative flex-1">
              <input
                id="admin-token"
                className="mc-input mono pr-9"
                type={reveal ? 'text' : 'password'}
                value={reveal ? auth.token : '•'.repeat(Math.min(32, Math.max(8, auth.token.length)))}
                readOnly
                onFocus={event => event.currentTarget.select()}
              />
              <button
                type="button"
                className="btn-icon absolute right-1 top-1/2 -translate-y-1/2"
                onClick={() => setReveal(value => !value)}
                aria-label={reveal ? 'Hide token' : 'Reveal token'}
                title={reveal ? 'Hide token' : 'Reveal session token'}
              >
                <Icon name="eye" size={15} />
              </button>
            </div>
          </div>
          <p className="field-help">Use this token for the REST API and CLI. It is held in this tab only — the daemon cannot show it again.</p>
          {daemon.data && (
            <p className="num mt-2 text-xs muted">fingerprint {daemon.data.token_fingerprint}</p>
          )}
          <div className="mt-3 flex flex-wrap gap-2">
            <button type="button" className="btn-sub" onClick={() => void copyToken()}>
              <Icon name={copied ? 'check' : 'copy'} size={14} />
              {copied ? 'Copied!' : 'Copy'}
            </button>
            <button type="button" className="btn-sub" onClick={() => void rotate()}>
              <Icon name="refresh" size={14} />
              Rotate
            </button>
          </div>
          {rotateError && (
            <p role="alert" className="mt-2 text-xs text-amber-400">{rotateError}</p>
          )}
        </Panel>

        {/* Diagnostics */}
        <Panel
          icon="wrench"
          title="Diagnostics"
          badge={
            <span className={`chip ${BADGE} ${failedChecks > 0 ? 'chip-error' : 'chip-success'}`}>
              {checks.length - failedChecks}/{checks.length}
            </span>
          }
        >
          <table className="mc-table">
            <tbody>
              {checks.map(check => (
                <tr key={check.name}>
                  <td>{check.name}</td>
                  <td className="text-right"><StatusChip ok={check.ok} detail={check.detail} /></td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              className="btn-sub"
              onClick={() => {
                void client.refetchQueries();
              }}
            >
              <Icon name="wrench" size={14} />
              Run doctor
            </button>
            <button
              type="button"
              className="btn-sub"
              onClick={() => void navigator.clipboard.writeText(`curl -s -H "Authorization: Bearer $MINICRON_TOKEN" http://127.0.0.1:7423/api/v1/daemon`)}
            >
              <Icon name="terminal" size={14} />
              Copy as curl
            </button>
          </div>
        </Panel>
      </div>

      {/* Right (8): recent activity and definition registry. */}
      <div className="flex min-w-0 flex-col gap-4 lg:col-span-8">
        {/* Activity tail (in place of a daemon log tail, which the API does not expose) */}
        <Panel
          icon="terminal"
          title="Recent activity"
          badge={<span className={`chip chip-neutral ${BADGE}`}>{activity.length}</span>}
          actions={
            <div className="flex items-center gap-3">
              <Link href="/runs" className="text-xs font-medium text-sky-300 hover:text-sky-200">
                View all runs
              </Link>
              <label className="flex items-center gap-2 text-xs muted">
                Live
                <span className="switch green">
                  <input type="checkbox" checked={liveTail} onChange={event => setLiveTail(event.target.checked)} />
                  <span className="track" />
                </span>
              </label>
            </div>
          }
        >
          <div className="log-view max-h-72 overflow-auto rounded-lg border border-base-300 p-3">
            {activity.length === 0 && <div className="faint">no recent runs</div>}
            {activity.map(run => {
              const failed = ['failed', 'timeout', 'interrupted'].includes(run.status);
              const level = failed ? 'WARN' : 'INFO';
              return (
                <div key={run.run_id} className="log-row !px-0">
                  <span className="log-ts">{formatDayTime(run.queued_at)}</span>
                  <span className={`log-tag !w-auto ${failed ? 'stderr' : 'system'}`}>{level}</span>
                  <span className="log-text">
                    <Link href={jobPath(run.job)} className="text-blue-300 hover:underline">
                      {run.job}
                    </Link>{' '}
                    {run.status} ({formatSpan(run.started_at, run.ended_at)})
                    {run.exit_code !== undefined && run.exit_code !== 0 ? ` · exit ${run.exit_code}` : ''} ·{' '}
                    <Link href={`/runs/${run.run_id}`} className="text-blue-300 hover:underline">
                      run {shortRunId(run.run_id)}
                    </Link>
                  </span>
                </div>
              );
            })}
          </div>
          {/* Poll faster while live tail is on. */}
          {liveTail && <LiveRefresher />}
        </Panel>

        {/* Definition registry */}
        <Panel
          icon="file-text"
          title="Definition registry"
          badge={<span className={`chip chip-neutral ${BADGE}`}>{definitions.length} definitions</span>}
          actions={
            <button
              type="button"
              className="btn-sub !py-1.5 !px-2.5 !text-xs"
              disabled={reload.isPending}
              onClick={() => reload.mutate(undefined)}
            >
              <Icon name="refresh" size={12} className={reload.isPending ? 'spin' : ''} />
              Reload settings & runtime
            </button>
          }
        >
          <div className="overflow-x-auto">
            <table className="mc-table">
              <thead>
                <tr>
                  <th>Registry</th>
                  <th>Definitions</th>
                  <th>Current status</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td className="font-mono text-[0.8rem]">SQLite</td>
                  <td className="num">{definitions.length}</td>
                  <td>
                    <span className={`chip chip-success ${BADGE}`}><Icon name="circle-check" size={11} /> Loaded</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          {reload.error ? (
            <p role="alert" className="mt-2 flex items-center gap-1.5 text-xs text-red-400">
              <Icon name="alert-circle" size={13} /> {errorText(reload.error)}
            </p>
          ) : (
            <p className="mt-2 flex items-center gap-1.5 text-xs text-green-400">
              <Icon name="circle-check" size={13} /> Runtime matches the definition registry.
              {reload.isSuccess && <span className="muted">Reloaded just now.</span>}
            </p>
          )}
        </Panel>
      </div>
    </div>
  );
}

/** Invisible component that keeps run data fresh while "Live" is enabled. */
function LiveRefresher() {
  useQuery({ ...runsQuery('', RECENT_RUNS_LIMIT), refetchInterval: 4000 });
  return null;
}

function ImportTab() {
  const client = useQueryClient();
  const [content, setContent] = useState('');
  const [preview, setPreview] = useState<{ hash: string; definitions: Definition[] } | null>(null);
  const [error, setError] = useState('');
  const [applied, setApplied] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const doPreview = async () => {
    setBusy(true);
    setError('');
    setApplied(null);
    try {
      const result = await api.importPreview(content);
      setPreview({ hash: result.content_hash, definitions: result.definitions });
    } catch (err) {
      setPreview(null);
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const doApply = async () => {
    if (!preview) return;
    setBusy(true);
    setError('');
    try {
      const result = await api.importApply(content, preview.hash);
      setApplied(result.applied);
      setPreview(null);
      void client.invalidateQueries({ queryKey: ['jobs'] });
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid gap-4 lg:grid-cols-12">
      <Panel icon="download" title="Export" badge={<span className={`chip chip-neutral ${BADGE}`}>toml · json</span>} className="lg:col-span-5">
        <p className="mb-3 text-sm muted">
          Download every definition as a TOML bundle or JSON document. Secrets are not included.
        </p>
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            className="btn-sub"
            onClick={() => void downloadFile('/api/v1/export', 'minicron-export.toml', api.download).catch(err => setError(errorText(err)))}
          >
            <Icon name="file-text" size={14} /> TOML bundle
          </button>
          <button
            type="button"
            className="btn-sub"
            onClick={() => void downloadFile('/api/v1/export?format=json', 'minicron-export.json', api.download).catch(err => setError(errorText(err)))}
          >
            <Icon name="file-text" size={14} /> JSON
          </button>
        </div>
      </Panel>

      <Panel
        icon="upload"
        title="Import"
        badge={preview ? <span className={`chip chip-info ${BADGE}`}>{preview.definitions.length} staged</span> : undefined}
        className="lg:col-span-7"
      >
        <p className="mb-3 text-sm muted">
          Paste a minicron TOML bundle, preview it, then apply it to the definition registry.
        </p>
        <input
          ref={fileRef}
          type="file"
          accept=".toml"
          className="hidden"
          onChange={async event => {
            const file = event.target.files?.[0];
            if (file) setContent(await file.text());
          }}
        />
        <button type="button" className="btn-sub mb-2" onClick={() => fileRef.current?.click()}>
          <Icon name="file-text" size={14} /> Load .toml file
        </button>
        <textarea
          className="mc-input min-h-[10rem]"
          value={content}
          onChange={event => setContent(event.target.value)}
          placeholder={'[[job]]\nname = "example"\nschedule = "@every 10m"\ncommand = "echo hello"'}
          spellCheck={false}
        />
        <div className="mt-3 flex items-center gap-2">
          <button type="button" className="btn-sub" disabled={!content.trim() || busy} onClick={() => void doPreview()}>
            {busy && <Icon name="loader" size={14} className="spin" />}
            Preview
          </button>
          {preview && (
            <button type="button" className="btn-primary-x" disabled={busy} onClick={() => void doApply()}>
              Apply {preview.definitions.length} definition{preview.definitions.length === 1 ? '' : 's'}
            </button>
          )}
        </div>
        {error && (
          <p role="alert" className="mt-2 text-xs text-red-400">{error}</p>
        )}
        {applied !== null && (
          <p role="status" className="mt-2 flex items-center gap-1.5 text-xs text-green-400">
            <Icon name="circle-check" size={13} /> Applied {applied} definition{applied === 1 ? '' : 's'}.
          </p>
        )}
        {preview && (
          <div className="mt-3 overflow-x-auto rounded-lg border border-base-300">
            <table className="mc-table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Schedule</th>
                </tr>
              </thead>
              <tbody>
                {preview.definitions.map(definition => (
                  <tr key={definition.name}>
                    <td className="font-mono text-[0.8rem]">{definition.name}</td>
                    <td>{definition.kind}</td>
                    <td className="font-mono text-[0.75rem]">{definition.schedule ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <div className="border-t border-base-300 px-3 py-1.5">
              <span className="num text-xs muted">sha256 {shortId(preview.hash, 24)}…</span>
            </div>
          </div>
        )}
      </Panel>
    </div>
  );
}
