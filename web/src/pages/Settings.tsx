import { useEffect, useMemo, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useSearch } from 'wouter';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import { api, errorText } from '../api';
import { auth } from '../auth';
import { daemonQuery, jobsQuery, runsQuery, useReloadDaemon } from '../queries';
import { downloadFile } from '../lib/download';
import { formatDayTime, formatSpan, shortId } from '../lib/format';
import { jobPath } from '../lib/routes';
import type { Definition } from '../types';

/** Settings & diagnostics + Export / Import tabs. */
export default function Settings() {
  const initialTab = new URLSearchParams(useSearch()).get('tab') === 'import' ? 'import' : 'main';
  const [tab, setTab] = useState<'main' | 'import'>(initialTab);
  useEffect(() => setTab(initialTab), [initialTab]);

  return (
    <div className="space-y-4">
      <PageHeader
        title="Settings & diagnostics"
        badge={<HealthBadge />}
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

function HealthBadge() {
  const daemon = useQuery(daemonQuery());
  if (daemon.isError) {
    return (
      <span className="chip chip-error">
        <span className="dot dot-red" /> Daemon unreachable
      </span>
    );
  }
  if (!daemon.data) {
    return (
      <span className="chip chip-neutral">
        <span className="dot dot-gray" /> Connecting…
      </span>
    );
  }
  return (
    <span className="chip chip-success">
      <span className="dot dot-green" /> Daemon healthy
    </span>
  );
}

function MainTab() {
  const daemon = useQuery(daemonQuery());
  const jobs = useQuery(jobsQuery());
  const runs = useQuery(runsQuery('', 100));
  const reload = useReloadDaemon();
  const client = useQueryClient();
  const [copied, setCopied] = useState(false);
  const [reveal, setReveal] = useState(false);
  const [rotateError, setRotateError] = useState('');
  const [liveTail, setLiveTail] = useState(true);

  const definitions = jobs.data ?? [];
  const sources = useMemo(() => {
    const byFile = new Map<string, number>();
    let dbCount = 0;
    for (const definition of definitions) {
      if (definition.authority === 'file' && definition.source_file) {
        byFile.set(definition.source_file, (byFile.get(definition.source_file) ?? 0) + 1);
      } else {
        dbCount += 1;
      }
    }
    return { files: [...byFile.entries()].sort(), dbCount };
  }, [definitions]);

  const checks = useMemo(
    () => [
      { name: 'Daemon API', ok: !daemon.isError && daemon.data !== undefined, detail: daemon.isError ? errorText(daemon.error) : `v${daemon.data?.version ?? ''}` },
      { name: 'Definitions readable', ok: !jobs.isError && jobs.data !== undefined, detail: jobs.isError ? errorText(jobs.error) : `${definitions.length} loaded` },
      { name: 'Run history readable', ok: !runs.isError && runs.data !== undefined, detail: runs.isError ? errorText(runs.error) : `${runs.data?.length ?? 0} recent runs` },
      { name: 'Log streaming', ok: !runs.isError, detail: runs.isError ? 'unavailable' : 'SSE ready' },
    ],
    [daemon, jobs, runs, definitions.length],
  );

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
          ? 'Token rotation requires the local Unix socket. Run `minicron token --rotate` on the host.'
          : errorText(error),
      );
    }
  };

  const activity = (runs.data ?? []).slice(0, 14);

  return (
    <div className="space-y-4">
      <div className="grid gap-4 lg:grid-cols-2">
        {/* Security */}
        <section className="panel p-4 sm:p-5">
          <h2 className="panel-title mb-3 flex items-center gap-2">
            <Icon name="key" size={15} className="text-blue-400" />
            Security
          </h2>
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
          <div className="mt-3 flex gap-2">
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
        </section>

        {/* Storage & retention */}
        <section className="panel p-4 sm:p-5">
          <h2 className="panel-title mb-3 flex items-center gap-2">
            <Icon name="hard-drive" size={15} className="text-blue-400" />
            Storage &amp; retention
          </h2>
          <table className="mc-table">
            <tbody>
              <tr>
                <td className="muted">Database</td>
                <td>SQLite (WAL)</td>
                <td className="text-right"><ChipHealthy ok={!daemon.isError} /></td>
              </tr>
              <tr>
                <td className="muted">Logs</td>
                <td>Local filesystem</td>
                <td className="text-right"><ChipHealthy ok={!runs.isError} /></td>
              </tr>
              <tr>
                <td className="muted">Schema</td>
                <td className="num">v{daemon.data?.schema_version ?? '—'}</td>
                <td className="text-right"><ChipHealthy ok={!daemon.isError} /></td>
              </tr>
              <tr>
                <td className="muted">Retention</td>
                <td>Per definition (keep_runs / keep_for)</td>
                <td className="text-right faint">—</td>
              </tr>
            </tbody>
          </table>
          <p className="field-help mt-2">Health reflects API reachability from this session.</p>
        </section>
      </div>

      {/* Import sources */}
      <section className="panel p-4 sm:p-5">
        <h2 className="panel-title mb-3 flex items-center gap-2">
          <Icon name="file-text" size={15} className="text-blue-400" />
          Import sources
        </h2>
        <div className="overflow-x-auto">
          <table className="mc-table">
            <thead>
              <tr>
                <th>Source</th>
                <th>Definitions</th>
                <th>Authority</th>
                <th>Current status</th>
                <th className="text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {sources.files.map(([file, count]) => (
                <tr key={file}>
                  <td className="font-mono text-[0.8rem]">{file}</td>
                  <td className="num">{count}</td>
                  <td>file</td>
                  <td>
                    <span className="chip chip-success"><Icon name="circle-check" size={12} /> Loaded</span>
                  </td>
                  <td className="text-right">
                    <button type="button" className="btn-sub !py-1 !px-2.5 !text-xs" disabled={reload.isPending} onClick={() => reload.mutate(undefined)}>
                      <Icon name="refresh" size={12} />
                      Reload
                    </button>
                  </td>
                </tr>
              ))}
              <tr>
                <td className="font-mono text-[0.8rem]">database (uploaded / API definitions)</td>
                <td className="num">{sources.dbCount}</td>
                <td>db</td>
                <td>
                  <span className="chip chip-success"><Icon name="circle-check" size={12} /> Loaded</span>
                </td>
                <td className="text-right">
                  <button type="button" className="btn-sub !py-1 !px-2.5 !text-xs" disabled={reload.isPending} onClick={() => reload.mutate(undefined)}>
                    <Icon name="refresh" size={12} />
                    Reload
                  </button>
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
            <Icon name="circle-check" size={13} /> All sources are up to date.
            {reload.isSuccess && <span className="muted">Reloaded just now.</span>}
          </p>
        )}
      </section>

      <div className="grid gap-4 lg:grid-cols-2">
        {/* Diagnostics */}
        <section className="panel p-4 sm:p-5">
          <h2 className="panel-title mb-3 flex items-center gap-2">
            <Icon name="wrench" size={15} className="text-blue-400" />
            Diagnostics
          </h2>
          <table className="mc-table">
            <thead>
              <tr>
                <th>Check</th>
                <th className="text-right">Status</th>
              </tr>
            </thead>
            <tbody>
              {checks.map(check => (
                <tr key={check.name}>
                  <td>{check.name}</td>
                  <td className="text-right">
                    <ChipHealthy ok={check.ok} detail={check.detail} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="mt-3 flex gap-2">
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
        </section>

        {/* Activity tail (in place of a daemon log tail, which the API does not expose) */}
        <section className="panel p-4 sm:p-5">
          <div className="mb-3 flex items-center justify-between">
            <h2 className="panel-title flex items-center gap-2">
              <Icon name="terminal" size={15} className="text-blue-400" />
              Recent activity
            </h2>
            <label className="flex items-center gap-2 text-xs muted">
              Live
              <span className="switch green">
                <input type="checkbox" checked={liveTail} onChange={event => setLiveTail(event.target.checked)} />
                <span className="track" />
              </span>
            </label>
          </div>
          <div className="log-view max-h-64 overflow-auto rounded-lg border border-base-300 p-3">
            {activity.length === 0 && <div className="faint">no recent runs</div>}
            {activity.map(run => {
              const failed = ['failed', 'timeout', 'interrupted'].includes(run.status);
              const level = failed ? 'WARN' : run.status === 'succeeded' ? 'INFO' : 'INFO';
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
                      run {shortId(run.run_id, 8)}
                    </Link>
                  </span>
                </div>
              );
            })}
          </div>
          <div className="mt-2 text-right">
            <Link href="/runs" className="text-xs text-sky-300 hover:text-sky-200">
              View all runs
            </Link>
          </div>
          {/* Poll faster while live tail is on. */}
          {liveTail && <LiveRefresher />}
        </section>
      </div>
    </div>
  );
}

/** Invisible component that keeps run data fresh while "Live" is enabled. */
function LiveRefresher() {
  useQuery({ ...runsQuery('', 100), refetchInterval: 4000 });
  return null;
}

function ChipHealthy({ ok, detail }: { ok: boolean; detail?: string }) {
  return (
    <span className={`chip ${ok ? 'chip-success' : 'chip-error'}`} title={detail}>
      <Icon name={ok ? 'circle-check' : 'alert-circle'} size={12} />
      {ok ? 'Healthy' : 'Error'}
    </span>
  );
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
    <div className="grid gap-4 lg:grid-cols-2">
      <section className="panel p-4 sm:p-5">
        <h2 className="panel-title mb-2 flex items-center gap-2">
          <Icon name="download" size={15} className="text-blue-400" />
          Export
        </h2>
        <p className="mb-3 text-sm muted">
          Download every definition as a TOML bundle or JSON document. Secrets are not included.
        </p>
        <div className="flex gap-2">
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
      </section>

      <section className="panel p-4 sm:p-5">
        <h2 className="panel-title mb-2 flex items-center gap-2">
          <Icon name="upload" size={15} className="text-blue-400" />
          Import
        </h2>
        <p className="mb-3 text-sm muted">
          Paste a minicron TOML bundle, preview it, then apply. Imported definitions become DB-authority copies.
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
      </section>
    </div>
  );
}
