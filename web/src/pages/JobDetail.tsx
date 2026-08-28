import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useLocation, useParams } from 'wouter';
import { errorText } from '../api';
import JobForm from '../components/JobForm';
import RunsTable from '../components/RunsTable';
import { jobQuery, runsQuery, useDeleteJob, useSaveJob, useSetJobEnabled, useTriggerJob, useWorkerAction } from '../queries';

export default function JobDetail() {
  const params = useParams();
  const name = params.name ?? '';
  const [, navigate] = useLocation();
  const detail = useQuery(jobQuery(name));
  const history = useQuery(runsQuery(name, 50));
  const trigger = useTriggerJob();
  const setEnabled = useSetJobEnabled();
  const save = useSaveJob();
  const remove = useDeleteJob();
  const workerAction = useWorkerAction();
  const [saveError, setSaveError] = useState('');

  if (detail.isPending) return <div className="skeleton h-64 w-full" />;
  if (detail.isError) {
    return (
      <div role="alert" className="alert alert-error">
        <span>Failed to load {name}: {errorText(detail.error)}</span>
      </div>
    );
  }

  const { definition, hash, active_runs, worker_state } = detail.data;
  const fileManaged = definition.authority === 'file';
  const enabled = definition.enabled !== false;
  const isWorker = definition.kind === 'worker';

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-2xl font-bold font-mono">{definition.name}</h1>
        <span className={`badge badge-sm ${isWorker ? 'badge-accent' : 'badge-neutral'}`}>{definition.kind}</span>
        <span className="badge badge-sm badge-ghost" title={fileManaged ? definition.source_file : undefined}>
          {fileManaged ? `file: ${definition.source_file}` : 'db authority'}
        </span>
        <span className={`badge badge-sm ${enabled ? 'badge-success' : 'badge-warning'}`}>{enabled ? 'enabled' : 'disabled'}</span>
        <span className="badge badge-sm badge-outline">rev {definition.revision ?? 0}</span>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <label className="label cursor-pointer gap-2">
            <span className="label-text">enabled</span>
            <input
              type="checkbox"
              className="toggle toggle-primary toggle-sm"
              checked={enabled}
              disabled={fileManaged || setEnabled.isPending}
              onChange={event => setEnabled.mutate({ name, enabled: event.target.checked })}
            />
          </label>
          {!isWorker && (
            <button
              type="button"
              className="btn btn-primary btn-sm"
              disabled={!enabled || trigger.isPending}
              onClick={() => trigger.mutate(name, { onSuccess: run => navigate(`/runs/${run.run_id}`) })}
            >
              {trigger.isPending ? <span className="loading loading-spinner loading-xs" /> : '▶'} Trigger run
            </button>
          )}
        </div>
      </div>

      {fileManaged && (
        <div role="alert" className="alert text-sm">
          <span>
            This definition is managed by <code className="font-mono">{definition.source_file}</code>. Edit that file
            and run <code className="font-mono">minicron reload</code>; API edits are rejected.
          </span>
        </div>
      )}

      {isWorker && worker_state && (
        <div className="stats stats-vertical border border-base-300 bg-base-100 shadow-sm sm:stats-horizontal">
          <div className="stat py-3">
            <div className="stat-title">State</div>
            <div className={`stat-value text-base ${worker_state.active ? 'text-success' : 'text-base-content/50'}`}>
              {worker_state.active ? 'running' : 'not running'}
            </div>
          </div>
          <div className="stat py-3">
            <div className="stat-title">Operator hold</div>
            <div className={`stat-value text-base ${worker_state.held ? 'text-warning' : ''}`}>{String(worker_state.held)}</div>
          </div>
          <div className="stat py-3">
            <div className="stat-title">Recent failures</div>
            <div className="stat-value text-base">{worker_state.failures}</div>
          </div>
          <div className="stat place-items-center py-3">
            <div className="flex gap-2">
              <button type="button" className="btn btn-xs" onClick={() => workerAction.mutate({ name, action: 'start' })}>
                Start
              </button>
              <button type="button" className="btn btn-xs" onClick={() => workerAction.mutate({ name, action: 'stop' })}>
                Hold
              </button>
              <button type="button" className="btn btn-xs" onClick={() => workerAction.mutate({ name, action: 'restart' })}>
                Restart
              </button>
            </div>
            {workerAction.error && <div className="stat-desc text-error">{errorText(workerAction.error)}</div>}
          </div>
        </div>
      )}

      <section className="space-y-3">
        <h2 className="text-lg font-semibold">Configuration</h2>
        <JobForm
          creating={false}
          initial={definition}
          revision={definition.revision}
          readOnly={fileManaged}
          submitting={save.isPending}
          error={saveError || (save.error ? errorText(save.error) : '')}
          onSubmit={(next: typeof definition, revision) => {
            setSaveError('');
            save.mutate(
              { name, definition: next, revision },
              {
                onSuccess: () => setSaveError(''),
                onError: err => setSaveError(errorText(err)),
              },
            );
          }}
        />
      </section>

      <section className="space-y-2">
        <h2 className="text-lg font-semibold">Run history</h2>
        <p className="text-xs text-base-content/60">
          {active_runs} active · definition hash <code className="font-mono">{hash ? hash.slice(0, 12) : '—'}</code>
        </p>
        {history.isPending ? <div className="skeleton h-40 w-full" /> : <RunsTable runs={history.data ?? []} />}
      </section>

      <section className="space-y-2">
        <h2 className="text-lg font-semibold text-error">Danger zone</h2>
        <div className="card border border-error/40 bg-base-100">
          <div className="card-body flex-row items-center justify-between gap-4 py-4">
            <p className="text-sm text-base-content/70">
              Removes the DB-authority definition. Past runs and logs are kept until retention cleans them.
            </p>
            <button
              type="button"
              className="btn btn-error btn-outline btn-sm"
              disabled={fileManaged || remove.isPending}
              onClick={() => {
                if (!window.confirm(`Delete definition "${name}"?`)) return;
                remove.mutate(name, { onSuccess: () => navigate('/') });
              }}
            >
              Delete definition
            </button>
          </div>
        </div>
      </section>
    </div>
  );
}
