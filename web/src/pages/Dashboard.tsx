import { useState } from 'react';
import { Link, useLocation } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { ApiError, api, errorText } from '../api';
import { downloadFile } from '../lib/download';
import { formatUptime } from '../lib/format';
import { daemonQuery, jobsQuery, runsQuery, useReloadDaemon, useSetJobEnabled, useTriggerJob } from '../queries';
import type { Definition, Run } from '../types';
import RunsTable from '../components/RunsTable';

const NEEDS_ATTENTION = new Set(['failed', 'timeout', 'interrupted']);

function ErrorAlert({ error }: { error: unknown }) {
  if (!error) return null;
  if (error instanceof ApiError && error.status === 401) return null; // handled globally
  return (
    <div role="alert" className="alert alert-error text-sm">
      <span>{errorText(error)}</span>
    </div>
  );
}

export default function Dashboard() {
  const [, navigate] = useLocation();
  const daemon = useQuery(daemonQuery());
  const jobs = useQuery(jobsQuery());
  const runs = useQuery(runsQuery('', 20));
  const trigger = useTriggerJob();
  const setEnabled = useSetJobEnabled();
  const reload = useReloadDaemon();
  const [notice, setNotice] = useState('');

  const definitions: Definition[] = jobs.data ?? [];
  const recentRuns: Run[] = runs.data ?? [];
  const active = recentRuns.filter(run => run.status === 'running').length;
  const attention = recentRuns.filter(run => NEEDS_ATTENTION.has(run.status)).length;

  async function handleExport(format: 'toml' | 'json') {
    try {
      await downloadFile(`/api/v1/export${format === 'json' ? '?format=json' : ''}`, `minicron-export.${format}`, api.download);
      setNotice('');
    } catch (err) {
      setNotice(errorText(err));
    }
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">Dashboard</h1>
        <div className="flex gap-2">
          <button type="button" className="btn btn-outline btn-sm" onClick={() => void handleExport('toml')}>
            Export TOML
          </button>
          <button type="button" className="btn btn-outline btn-sm" onClick={() => void reload.mutate(undefined)}>
            {reload.isPending && <span className="loading loading-spinner loading-xs" />}
            Reload config
          </button>
        </div>
      </div>

      {(reload.error || notice) && <ErrorAlert error={reload.error ?? notice} />}

      <div className="stats stats-vertical w-full border border-base-300 bg-base-100 shadow-sm sm:stats-horizontal">
        <div className="stat">
          <div className="stat-title">Daemon</div>
          <div className="stat-value text-lg">{daemon.data ? `v${daemon.data.version}` : '—'}</div>
          <div className="stat-desc">
            {daemon.data ? (
              <>
                schema {daemon.data.schema_version} · up {formatUptime(daemon.data.uptime_s)}
              </>
            ) : (
              'connecting…'
            )}
          </div>
        </div>
        <div className="stat">
          <div className="stat-title">Token fingerprint</div>
          <div className="stat-value text-lg font-mono">{daemon.data?.token_fingerprint ?? '—'}</div>
          <div className="stat-desc">rotate locally via the Unix socket</div>
        </div>
        <div className="stat">
          <div className="stat-title">Definitions</div>
          <div className="stat-value text-lg">{definitions.length}</div>
          <div className="stat-desc">{definitions.filter(d => d.kind === 'worker').length} workers</div>
        </div>
        <div className="stat">
          <div className="stat-title">Active runs</div>
          <div className={`stat-value text-lg ${active > 0 ? 'text-info' : ''}`}>{active}</div>
          <div className="stat-desc">{attention} need attention</div>
        </div>
      </div>

      <section className="space-y-2">
        <h2 className="text-lg font-semibold">Definitions</h2>
        <ErrorAlert error={jobs.error} />
        {jobs.isPending ? (
          <div className="skeleton h-40 w-full" />
        ) : definitions.length === 0 ? (
          <div className="rounded-box border border-base-300 bg-base-100 p-8 text-center text-base-content/60">
            No definitions yet. <Link href="/jobs/new" className="link link-primary">Create one</Link> or add jobs to
            your TOML config and reload.
          </div>
        ) : (
          <div className="overflow-x-auto rounded-box border border-base-300 bg-base-100">
            <table className="table table-sm">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Schedule</th>
                  <th>Authority</th>
                  <th>Enabled</th>
                  <th className="text-right">Actions</th>
                </tr>
              </thead>
              <tbody>
                {definitions.map(definition => {
                  const fileManaged = definition.authority === 'file';
                  const enabled = definition.enabled !== false;
                  return (
                    <tr key={definition.name} className="hover">
                      <td>
                        <Link href={`/jobs/${definition.name}`} className="link link-hover font-semibold">
                          {definition.name}
                        </Link>
                      </td>
                      <td>
                        <span className={`badge badge-sm ${definition.kind === 'worker' ? 'badge-accent' : 'badge-neutral'}`}>
                          {definition.kind}
                        </span>
                      </td>
                      <td className="font-mono text-xs">{definition.schedule || (definition.kind === 'worker' ? 'always-on' : '—')}</td>
                      <td>
                        <span className="badge badge-ghost badge-sm" title={definition.source_file}>
                          {fileManaged ? `file: ${definition.source_file}` : 'db'}
                        </span>
                      </td>
                      <td>
                        <input
                          type="checkbox"
                          className="toggle toggle-primary toggle-sm"
                          checked={enabled}
                          disabled={fileManaged || setEnabled.isPending}
                          onChange={event => setEnabled.mutate({ name: definition.name, enabled: event.target.checked })}
                        />
                      </td>
                      <td className="text-right">
                        {definition.kind === 'job' && (
                          <button
                            type="button"
                            className="btn btn-primary btn-xs"
                            disabled={!enabled || trigger.isPending}
                            onClick={() => trigger.mutate(definition.name, { onSuccess: run => navigate(`/runs/${run.run_id}`) })}
                          >
                            Run
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="space-y-2">
        <h2 className="text-lg font-semibold">Recent runs</h2>
        <ErrorAlert error={runs.error} />
        {runs.isPending ? <div className="skeleton h-40 w-full" /> : <RunsTable runs={recentRuns} />}
      </section>
    </div>
  );
}
