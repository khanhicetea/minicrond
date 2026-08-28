import type { FormEvent } from 'react';
import type { Definition } from '../types';

interface JobFormProps {
  creating: boolean;
  initial: Definition;
  revision?: number;
  readOnly: boolean;
  submitting: boolean;
  error?: string | null;
  onSubmit: (definition: Definition, revision?: number) => void;
  onCancel?: () => void;
}

export function emptyDefinition(kind: 'job' | 'worker' = 'job'): Definition {
  const base: Definition = {
    name: '',
    kind,
    enabled: true,
    timezone: 'UTC',
    env_base: 'clean',
  };
  if (kind === 'job') {
    base.schedule = '';
    base.catch_up = 'none';
    base.on_overlap = 'skip';
    base.run_on_start = false;
  } else {
    base.autostart = true;
    base.restart = 'always';
    base.restart_delay = '5s';
    base.max_restart_attempts = 0;
  }
  return base;
}

function parseKeyValue(text: string): Record<string, string> {
  const result: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const eq = line.indexOf('=');
    if (eq <= 0) continue;
    result[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return result;
}

function textOrUndefined(value: FormDataEntryValue | null): string | undefined {
  const text = String(value ?? '').trim();
  return text || undefined;
}

/** Structured editor for a job or worker definition. */
export default function JobForm({ creating, initial, revision, readOnly, submitting, error, onSubmit, onCancel }: JobFormProps) {
  const isJob = (initial.kind ?? 'job') !== 'worker';

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (readOnly || submitting) return;
    const form = new FormData(event.currentTarget);
    const kind = String(form.get('kind') ?? 'job') === 'worker' ? 'worker' : 'job';
    const command = textOrUndefined(form.get('command'));
    const argv = String(form.get('argv') ?? '')
      .split('\n')
      .map(line => line.trim())
      .filter(Boolean);

    const definition: Definition = {
      name: String(form.get('name') ?? '').trim(),
      kind,
      enabled: form.get('enabled') === 'on',
      schedule: kind === 'job' ? textOrUndefined(form.get('schedule')) : undefined,
      timezone: textOrUndefined(form.get('timezone')) ?? 'UTC',
      catch_up: kind === 'job' ? String(form.get('catch_up') ?? 'none') : undefined,
      on_overlap: kind === 'job' ? String(form.get('on_overlap') ?? 'skip') : undefined,
      run_on_start: kind === 'job' ? form.get('run_on_start') === 'on' : undefined,
      timeout: textOrUndefined(form.get('timeout')),
      grace: textOrUndefined(form.get('grace')),
      stop_signal: textOrUndefined(form.get('stop_signal')),
      run_as: textOrUndefined(form.get('run_as')),
      working_dir: textOrUndefined(form.get('working_dir')),
      env_base: textOrUndefined(form.get('env_base')) ?? 'clean',
      env_file: textOrUndefined(form.get('env_file')),
      keep_runs: Number(form.get('keep_runs') ?? 0) || 0,
      keep_for: textOrUndefined(form.get('keep_for')),
      log_max: textOrUndefined(form.get('log_max')),
    };
    // Exactly one of command/argv must be set.
    if (command) definition.command = command;
    else definition.argv = argv;

    const env = parseKeyValue(String(form.get('env') ?? ''));
    if (Object.keys(env).length > 0) definition.env = env;
    const secretEnv = parseKeyValue(String(form.get('secret_env') ?? ''));
    if (Object.keys(secretEnv).length > 0) definition.secret_env = secretEnv;

    const successCodes = String(form.get('success_codes') ?? '')
      .split(',')
      .map(part => Number(part.trim()))
      .filter(code => Number.isInteger(code));
    if (successCodes.length > 0) definition.success_codes = successCodes;

    if (kind === 'worker') {
      definition.autostart = form.get('autostart') === 'on';
      definition.restart = textOrUndefined(form.get('restart'));
      definition.restart_delay = textOrUndefined(form.get('restart_delay'));
      definition.max_restart_attempts = Number(form.get('max_restart_attempts') ?? 0) || 0;
      definition.healthy_after = textOrUndefined(form.get('healthy_after'));
      definition.priority = Number(form.get('priority') ?? 0) || 0;
    }

    onSubmit(definition, revision);
  }

  const field = 'input input-sm input-bordered w-full';
  const label = 'label label-text text-xs font-semibold';

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      <div className="card border border-base-300 bg-base-100">
        <div className="card-body gap-4">
          <h3 className="card-title text-sm uppercase tracking-wide text-base-content/70">Identity</h3>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label className="form-control">
              <span className={label}>Name</span>
              <input
                name="name"
                required
                pattern="[a-z0-9][a-z0-9_.\-]{0,99}"
                defaultValue={initial.name}
                readOnly={!creating}
                disabled={readOnly}
                className={field}
                placeholder="nightly-backup"
              />
            </label>
            <label className="form-control">
              <span className={label}>Kind</span>
              <select name="kind" defaultValue={initial.kind} disabled={!creating || readOnly} className="select select-sm select-bordered w-full">
                <option value="job">job</option>
                <option value="worker">worker</option>
              </select>
            </label>
            <label className="label cursor-pointer justify-start gap-3 pt-6">
              <input name="enabled" type="checkbox" defaultChecked={initial.enabled !== false} disabled={readOnly} className="toggle toggle-sm toggle-primary" />
              <span className="label-text">Enabled</span>
            </label>
          </div>

          {isJob ? (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <label className="form-control sm:col-span-2">
                <span className={label}>Schedule (cron, or `@every 1h`)</span>
                <input name="schedule" defaultValue={initial.schedule ?? ''} disabled={readOnly} className={field} placeholder="*/15 * * * *" />
              </label>
              <label className="form-control">
                <span className={label}>Timezone</span>
                <input name="timezone" defaultValue={initial.timezone ?? 'UTC'} disabled={readOnly} className={field} />
              </label>
              <label className="form-control">
                <span className={label}>Catch up</span>
                <select name="catch_up" defaultValue={initial.catch_up ?? 'none'} disabled={readOnly} className="select select-sm select-bordered w-full">
                  <option value="none">none</option>
                  <option value="latest">latest</option>
                </select>
              </label>
              <label className="form-control">
                <span className={label}>On overlap</span>
                <select name="on_overlap" defaultValue={initial.on_overlap ?? 'skip'} disabled={readOnly} className="select select-sm select-bordered w-full">
                  <option value="skip">skip</option>
                  <option value="parallel">parallel</option>
                </select>
              </label>
              <label className="label cursor-pointer justify-start gap-3 self-end pb-2">
                <input name="run_on_start" type="checkbox" defaultChecked={initial.run_on_start === true} disabled={readOnly} className="toggle toggle-sm toggle-primary" />
                <span className="label-text">Run on daemon start</span>
              </label>
            </div>
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <label className="form-control">
                <span className={label}>Restart policy</span>
                <select name="restart" defaultValue={initial.restart ?? 'always'} disabled={readOnly} className="select select-sm select-bordered w-full">
                  <option value="always">always</option>
                  <option value="on-failure">on-failure</option>
                  <option value="never">never</option>
                </select>
              </label>
              <label className="form-control">
                <span className={label}>Restart delay</span>
                <input name="restart_delay" defaultValue={initial.restart_delay ?? '5s'} disabled={readOnly} className={field} placeholder="5s" />
              </label>
              <label className="form-control">
                <span className={label}>Max restart attempts</span>
                <input name="max_restart_attempts" type="number" min={0} defaultValue={initial.max_restart_attempts ?? 0} disabled={readOnly} className={field} />
              </label>
              <label className="form-control">
                <span className={label}>Healthy after</span>
                <input name="healthy_after" defaultValue={initial.healthy_after ?? ''} disabled={readOnly} className={field} placeholder="10s" />
              </label>
              <label className="label cursor-pointer justify-start gap-3 self-end pb-2">
                <input name="autostart" type="checkbox" defaultChecked={initial.autostart !== false} disabled={readOnly} className="toggle toggle-sm toggle-primary" />
                <span className="label-text">Autostart with daemon</span>
              </label>
              <label className="form-control">
                <span className={label}>Priority</span>
                <input name="priority" type="number" defaultValue={initial.priority ?? 0} disabled={readOnly} className={field} />
              </label>
            </div>
          )}
        </div>
      </div>

      <div className="card border border-base-300 bg-base-100">
        <div className="card-body gap-4">
          <h3 className="card-title text-sm uppercase tracking-wide text-base-content/70">Command</h3>
          <p className="text-xs text-base-content/60">
            Exactly one of <strong>shell command</strong> or <strong>argv</strong> must be filled.
          </p>
          <div className="grid gap-4 lg:grid-cols-2">
            <label className="form-control">
              <span className={label}>Command (run with `shell -c`)</span>
              <textarea
                name="command"
                defaultValue={initial.command ?? ''}
                disabled={readOnly}
                className="textarea textarea-bordered min-h-24 font-mono text-sm"
                placeholder="echo hello"
              />
            </label>
            <label className="form-control">
              <span className={label}>Argv (one argument per line)</span>
              <textarea
                name="argv"
                defaultValue={(initial.argv ?? []).join('\n')}
                disabled={readOnly}
                className="textarea textarea-bordered min-h-24 font-mono text-sm"
                placeholder={'/usr/bin/curl\n-fsS\nhttps://example.com'}
              />
            </label>
          </div>
          <div className="grid gap-4 sm:grid-cols-3">
            <label className="form-control">
              <span className={label}>Timeout</span>
              <input name="timeout" defaultValue={initial.timeout ?? ''} disabled={readOnly} className={field} placeholder="30s" />
            </label>
            <label className="form-control">
              <span className={label}>Grace before SIGKILL</span>
              <input name="grace" defaultValue={initial.grace ?? ''} disabled={readOnly} className={field} placeholder="5s" />
            </label>
            <label className="form-control">
              <span className={label}>Stop signal</span>
              <input name="stop_signal" defaultValue={initial.stop_signal ?? ''} disabled={readOnly} className={field} placeholder="TERM" />
            </label>
          </div>
        </div>
      </div>

      <details className="collapse collapse-arrow border border-base-300 bg-base-100">
        <summary className="collapse-title text-sm uppercase tracking-wide text-base-content/70">Environment &amp; retention</summary>
        <div className="collapse-content space-y-4">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label className="form-control">
              <span className={label}>Run as (user or UID)</span>
              <input name="run_as" defaultValue={initial.run_as ?? ''} disabled={readOnly} className={field} placeholder="alice / 1000" />
            </label>
            <label className="form-control">
              <span className={label}>Working directory</span>
              <input name="working_dir" defaultValue={initial.working_dir ?? ''} disabled={readOnly} className={field} />
            </label>
            <label className="form-control">
              <span className={label}>Environment base</span>
              <select name="env_base" defaultValue={initial.env_base ?? 'clean'} disabled={readOnly} className="select select-sm select-bordered w-full">
                <option value="clean">clean</option>
                <option value="minimal">minimal</option>
              </select>
            </label>
            <label className="form-control">
              <span className={label}>Env file</span>
              <input name="env_file" defaultValue={initial.env_file ?? ''} disabled={readOnly} className={field} />
            </label>
          </div>
          <div className="grid gap-4 lg:grid-cols-2">
            <label className="form-control">
              <span className={label}>Environment (KEY=VALUE per line)</span>
              <textarea
                name="env"
                defaultValue={Object.entries(initial.env ?? {})
                  .map(([key, value]) => `${key}=${value}`)
                  .join('\n')}
                disabled={readOnly}
                className="textarea textarea-bordered min-h-20 font-mono text-sm"
                placeholder={'FOO=bar\nBAZ=qux'}
              />
            </label>
            <label className="form-control">
              <span className={label}>Secret environment (KEY=VALUE per line)</span>
              <textarea
                name="secret_env"
                defaultValue={Object.entries(initial.secret_env ?? {})
                  .map(([key, value]) => `${key}=${value}`)
                  .join('\n')}
                disabled={readOnly}
                className="textarea textarea-bordered min-h-20 font-mono text-sm"
              />
            </label>
          </div>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label className="form-control">
              <span className={label}>Keep runs</span>
              <input name="keep_runs" type="number" min={0} defaultValue={initial.keep_runs ?? 0} disabled={readOnly} className={field} />
            </label>
            <label className="form-control">
              <span className={label}>Keep for</span>
              <input name="keep_for" defaultValue={initial.keep_for ?? ''} disabled={readOnly} className={field} placeholder="720h" />
            </label>
            <label className="form-control">
              <span className={label}>Log max size</span>
              <input name="log_max" defaultValue={initial.log_max ?? ''} disabled={readOnly} className={field} placeholder="10MiB" />
            </label>
            <label className="form-control">
              <span className={label}>Success codes</span>
              <input
                name="success_codes"
                defaultValue={(initial.success_codes ?? []).join(',')}
                disabled={readOnly}
                className={field}
                placeholder="0"
              />
            </label>
          </div>
        </div>
      </details>

      {error && (
        <div role="alert" className="alert alert-error text-sm">
          <span>{error}</span>
        </div>
      )}

      {!readOnly && (
        <div className="flex items-center gap-2">
          <button type="submit" disabled={submitting} className="btn btn-primary btn-sm">
            {submitting && <span className="loading loading-spinner loading-xs" />}
            {creating ? 'Create definition' : 'Save changes'}
          </button>
          {onCancel && (
            <button type="button" className="btn btn-ghost btn-sm" onClick={onCancel}>
              Cancel
            </button>
          )}
        </div>
      )}
    </form>
  );
}
