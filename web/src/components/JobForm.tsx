import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, errorText } from '../api';
import { alertChannelsQuery } from '../queries';
import { Icon } from './Icon';
import type { Definition } from '../types';
import { buildCron, cronSelectValues, humanizeSchedule, nextFires, parseSchedule } from '../lib/cron';
import { TIMEZONES } from '../lib/timezones';

export function emptyDefinition(kind: 'job' | 'worker' = 'job'): Definition {
  const base: Definition = {
    name: '',
    kind,
    enabled: true,
    timezone: 'UTC',
    env_base: 'clean',
    timeout: 0,
    grace: 10,
  };
  if (kind === 'job') {
    base.schedule = '0 0 * * *';
    base.catch_up = 'none';
    base.on_overlap = 'skip';
    base.retries = 0;
    base.retry_delay = 5;
    base.run_on_start = false;
  } else {
    base.autostart = true;
    base.restart = 'always';
    base.restart_delay = 5;
    base.max_restart_attempts = 5;
    base.healthy_after = 30;
  }
  return base;
}

const MINUTE_OPTIONS = ['*', '*/5', '*/10', '*/15', '*/30', ...Array.from({ length: 60 }, (_, i) => String(i))];
const HOUR_OPTIONS = ['*', '*/2', '*/4', '*/6', '*/12', ...Array.from({ length: 24 }, (_, i) => String(i))];
const DOM_OPTIONS = ['*', ...Array.from({ length: 31 }, (_, i) => String(i + 1))];
const MONTH_OPTIONS = ['*', ...Array.from({ length: 12 }, (_, i) => String(i + 1))];
const DOW_OPTIONS = ['*', '0', '1', '2', '3', '4', '5', '6'];
const DOW_LABEL: Record<string, string> = { '*': 'any', '0': '0 (Sun)', '1': '1 (Mon)', '2': '2 (Tue)', '3': '3 (Wed)', '4': '4 (Thu)', '5': '5 (Fri)', '6': '6 (Sat)' };

interface JobFormProps {
  /** Show the Job/Worker switcher (blank "new definition" only). */
  showKindTabs: boolean;
  /** Lock the name field (editing an existing definition). */
  nameLocked: boolean;
  draft: Definition;
  readOnly: boolean;
  runAsEnabled: boolean;
  onChange: (patch: Partial<Definition>) => void;
  formId: string;
}

/**
 * Definition editor form (left column of the editor page). Controlled: the
 * page owns the draft so the TOML panel can mirror it live.
 */
export default function JobForm({ showKindTabs, nameLocked, draft, readOnly, runAsEnabled, onChange, formId }: JobFormProps) {
  const isJob = draft.kind !== 'worker';
  const disabled = readOnly;
  const scheduleType = (draft.schedule ?? '').trim().toLowerCase().startsWith('@every') ? 'every' : 'cron';
  const tzOptions = TIMEZONES;
  const channels = useQuery(alertChannelsQuery());
  const [testing, setTesting] = useState('');
  const [testResult, setTestResult] = useState('');
  const selectedAlerts = draft.alerts ?? [];
  const configuredChannels = channels.data ?? [];
  // Keep saved names that are no longer configured available for removal.
  const channelNames = [...new Set([...configuredChannels.map(channel => channel.name), ...selectedAlerts])];

  const setKind = (kind: 'job' | 'worker') => {
    onChange({ ...emptyDefinition(kind), name: draft.name, retries: kind === 'worker' ? undefined : 0, retry_delay: kind === 'worker' ? undefined : 5 });
  };

  const cronParts = cronSelectValues(draft.schedule ?? '');
  const setCronPart = (index: number, value: string) => {
    const parts = [...cronParts];
    parts[index] = value;
    onChange({ schedule: buildCron(parts[0], parts[1], parts[2], parts[3], parts[4]) });
  };

  // Ticking clock for the "next firings" helper.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, []);

  const tz = draft.timezone || 'UTC';
  const nextFireList = isJob && parseSchedule(draft.schedule ?? '') ? nextFires(draft.schedule!, 5, now, tz) : [];

  const field = 'mc-input';
  const label = 'field-label';

  return (
    <form id={formId} onSubmit={event => event.preventDefault()} className="space-y-4" aria-label="Definition editor">
      {/* Identity & execution */}
      <section className="panel p-4 sm:p-5">
        {showKindTabs && (
          <div className="tab-seg mb-4">
            <button type="button" className={isJob ? 'active' : ''} onClick={() => setKind('job')} disabled={disabled}>
              Job
            </button>
            <button type="button" className={!isJob ? 'active' : ''} onClick={() => setKind('worker')} disabled={disabled}>
              Worker
            </button>
          </div>
        )}

        <div className="grid gap-4">
          <div>
            <label htmlFor={`${formId}-name`} className={label}>Name</label>
            <input
              id={`${formId}-name`}
              className={`${field} mono`}
              value={draft.name}
              readOnly={nameLocked || disabled}
              disabled={disabled}
              onChange={event => onChange({ name: event.target.value })}
              placeholder="database-backup"
              autoComplete="off"
              spellCheck={false}
            />
            <p className="field-help">Unique name for this definition.</p>
          </div>

          <div>
            <label htmlFor={`${formId}-command`} className={label}>Command</label>
            <input
              id={`${formId}-command`}
              className={`${field} mono`}
              value={draft.command ?? ''}
              disabled={disabled}
              onChange={event => onChange({ command: event.target.value, argv: undefined })}
              placeholder="pg_dump --format=custom appdb"
              autoComplete="off"
              spellCheck={false}
            />
            <p className="field-help">Command to execute (run with shell -c). argv can be set under Advanced.</p>
          </div>

          {isJob ? (
            <>
              <div className="grid gap-4 lg:grid-cols-[0.55fr_1.45fr]">
                <div>
                  <label htmlFor={`${formId}-schtype`} className={label}>Schedule type</label>
                  <select
                    id={`${formId}-schtype`}
                    className="mc-select"
                    value={scheduleType}
                    disabled={disabled}
                    onChange={event => {
                      if (event.target.value === 'every') onChange({ schedule: '@every 15m' });
                      else onChange({ schedule: '0 0 * * *' });
                    }}
                  >
                    <option value="cron">Cron expression</option>
                    <option value="every">Interval (@every)</option>
                  </select>
                </div>
                <div>
                  <label htmlFor={`${formId}-sched`} className={label}>
                    {scheduleType === 'every' ? 'Interval' : 'Cron expression'}
                  </label>
                  <input
                    id={`${formId}-sched`}
                    className={`${field} mono`}
                    value={draft.schedule ?? ''}
                    disabled={disabled}
                    onChange={event => onChange({ schedule: event.target.value })}
                    placeholder={scheduleType === 'every' ? '@every 15m' : '0 2 * * *'}
                    spellCheck={false}
                  />
                  <p className="field-help">{humanizeSchedule(draft.schedule ?? '')}</p>
                </div>
              </div>

              {/* Cron helper: full width under the schedule row, fields 9/12 + preview 3/12. */}
              {scheduleType === 'cron' && (
                <div className="rounded-xl border border-base-300 bg-base-200/35 p-3">
                  <div className="mb-2 flex items-center gap-2">
                    <span className="text-xs font-semibold uppercase tracking-wide muted">Cron helper</span>
                    <Icon name="info" size={13} className="faint" />
                  </div>
                  <div className="grid gap-3 lg:grid-cols-12">
                    <div className="lg:col-span-9">
                      <div className="grid grid-cols-5 gap-2 overflow-x-auto">
                          {(
                            [
                              ['Minute', MINUTE_OPTIONS, 0],
                              ['Hour', HOUR_OPTIONS, 1],
                              ['Day', DOM_OPTIONS, 2],
                              ['Month', MONTH_OPTIONS, 3],
                              ['Weekday', DOW_OPTIONS, 4],
                            ] as [string, string[], number][]
                          ).map(([name, options, index]) => (
                            <div key={name}>
                              <label htmlFor={`${formId}-cron-${index}`} className="mb-1 block text-[0.65rem] font-semibold uppercase tracking-wide muted">{name}</label>
                              <select
                                id={`${formId}-cron-${index}`}
                                className="mc-select mono !min-h-9 !text-xs"
                                value={cronParts[index] ?? '*'}
                                disabled={disabled}
                                onChange={event => setCronPart(index, event.target.value)}
                              >
                                {options.map(option => (
                                  <option key={option} value={option}>
                                    {index === 4 ? (DOW_LABEL[option] ?? option) : option}
                                  </option>
                                ))}
                              </select>
                            </div>
                          ))}
                      </div>
                    </div>
                    <div className="lg:col-span-3">
                      <div className="h-full rounded-lg bg-base-100/45 p-2.5">
                        <h4 className="mb-1.5 text-[0.65rem] font-semibold uppercase tracking-wide muted">Next 5 fires · {tz}</h4>
                          {nextFireList.length > 0 ? (
                            <ul className="grid gap-1">
                              {nextFireList.map((fire, index) => (
                                <li key={fire} className="flex items-center gap-1.5 text-xs">
                                  <Icon name="calendar" size={12} className="faint shrink-0" />
                                  <span className="num">
                                    {new Date(fire).toLocaleString(undefined, {
                                      month: 'short',
                                      day: 'numeric',
                                      hour: '2-digit',
                                      minute: '2-digit',
                                    })}
                                  </span>
                                  {index === 0 && <span className="chip chip-info !px-1.5 !py-0 !text-[0.6rem]">next</span>}
                                </li>
                              ))}
                            </ul>
                          ) : (
                            <p className="text-xs faint">Enter a valid cron expression to preview firings.</p>
                          )}
                      </div>
                    </div>
                  </div>
                </div>
              )}

              <div className="grid gap-4 sm:grid-cols-2">
                <div>
                  <label htmlFor={`${formId}-tz`} className={label}>Timezone</label>
                  <select
                    id={`${formId}-tz`}
                    className="mc-select mono"
                    value={draft.timezone ?? 'UTC'}
                    disabled={disabled}
                    onChange={event => onChange({ timezone: event.target.value })}
                  >
                    {tzOptions.map(zone => (
                      <option key={zone} value={zone}>
                        {zone}
                      </option>
                    ))}
                  </select>
                </div>
                <div>
                  <label htmlFor={`${formId}-runas`} className={label}>Run as</label>
                  <input
                    id={`${formId}-runas`}
                    className={field}
                    value={draft.run_as ?? ''}
                    disabled={disabled || !runAsEnabled}
                    onChange={event => onChange({ run_as: event.target.value || undefined })}
                    placeholder="backup"
                  />
                  <p className="field-help">
                    {runAsEnabled ? 'System user to run the command as.' : 'Available only when the daemon runs as root.'}
                  </p>
                </div>
              </div>

              <div className="grid gap-4 sm:grid-cols-3">
                <div>
                  <label htmlFor={`${formId}-timeout`} className={label}>Timeout (seconds)</label>
                  <input
                    id={`${formId}-timeout`}
                    type="number"
                    min={0}
                    step={1}
                    className={field}
                    value={draft.timeout ?? ''}
                    disabled={disabled}
                    onChange={event => onChange({ timeout: optionalNumber(event.target.value) })}
                    placeholder="1200"
                  />
                  <p className="field-help">Max runtime per run; 0 disables the timeout</p>
                </div>
                <div>
                  <label htmlFor={`${formId}-grace`} className={label}>Grace (seconds)</label>
                  <input
                    id={`${formId}-grace`}
                    type="number"
                    min={1}
                    step={1}
                    className={field}
                    value={draft.grace ?? ''}
                    disabled={disabled}
                    onChange={event => onChange({ grace: optionalNumber(event.target.value) })}
                    placeholder="10"
                  />
                  <p className="field-help">Before SIGKILL</p>
                </div>
                <div>
                  <label htmlFor={`${formId}-overlap`} className={label}>Overlap policy</label>
                  <select
                    id={`${formId}-overlap`}
                    className="mc-select"
                    value={draft.on_overlap ?? 'skip'}
                    disabled={disabled}
                    onChange={event => onChange({ on_overlap: event.target.value })}
                  >
                    <option value="skip">Skip</option>
                    <option value="parallel">Parallel</option>
                  </select>
                  <p className="field-help">If a run is already active</p>
                </div>
              </div>
              <div className="grid gap-4 sm:grid-cols-2">
                <div>
                  <label htmlFor={`${formId}-retries`} className={label}>Retries after failure</label>
                  <input id={`${formId}-retries`} type="number" min={0} max={1000} step={1} className={field}
                    value={draft.retries ?? 0} disabled={disabled}
                    onChange={event => onChange({ retries: optionalNumber(event.target.value) })} />
                  <p className="field-help">Additional attempts; timeouts and stopped runs are not retried</p>
                </div>
                <div>
                  <label htmlFor={`${formId}-retry-delay`} className={label}>Retry delay (seconds)</label>
                  <input id={`${formId}-retry-delay`} type="number" min={0} max={86400} step={1} className={field}
                    value={draft.retry_delay ?? 5} disabled={disabled}
                    onChange={event => onChange({ retry_delay: optionalNumber(event.target.value) })} />
                </div>
              </div>
            </>
          ) : (
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <div>
                <label htmlFor={`${formId}-restart`} className={label}>Restart policy</label>
                <select
                  id={`${formId}-restart`}
                  className="mc-select"
                  value={draft.restart ?? 'always'}
                  disabled={disabled}
                  onChange={event => onChange({ restart: event.target.value })}
                >
                  <option value="always">Always</option>
                  <option value="on-failure">On failure</option>
                  <option value="never">Never</option>
                </select>
              </div>
              <div>
                <label htmlFor={`${formId}-rdelay`} className={label}>Restart delay (seconds)</label>
                <input
                  id={`${formId}-rdelay`}
                  type="number"
                  min={1}
                  step={1}
                  className={field}
                  value={draft.restart_delay ?? 5}
                  disabled={disabled}
                  onChange={event => onChange({ restart_delay: optionalNumber(event.target.value) })}
                  placeholder="5"
                />
              </div>
              <div>
                <label htmlFor={`${formId}-rmax`} className={label}>Max restart attempts</label>
                <input
                  id={`${formId}-rmax`}
                  type="number"
                  min={0}
                  className={field}
                  value={draft.max_restart_attempts ?? 0}
                  disabled={disabled}
                  onChange={event => onChange({ max_restart_attempts: Number(event.target.value) || 0 })}
                />
              </div>
              <div>
                <label htmlFor={`${formId}-healthy`} className={label}>Healthy after (seconds)</label>
                <input
                  id={`${formId}-healthy`}
                  type="number"
                  min={1}
                  step={1}
                  className={field}
                  value={draft.healthy_after ?? ''}
                  disabled={disabled}
                  onChange={event => onChange({ healthy_after: optionalNumber(event.target.value) })}
                  placeholder="30"
                />
              </div>
              <label className="flex items-center gap-2.5 text-sm">
                <span className="switch">
                  <input
                    type="checkbox"
                    checked={draft.autostart !== false}
                    disabled={disabled}
                    onChange={event => onChange({ autostart: event.target.checked })}
                  />
                  <span className="track" />
                </span>
                Autostart with daemon
              </label>
            </div>
          )}
        </div>
      </section>

      {/* Advanced */}
      <details className="panel group px-4 py-3 sm:px-5">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 text-sm font-semibold">
          <span className="flex items-center gap-2">
            <Icon name="chevron-right" size={14} className="transition-transform group-open:rotate-90 muted" />
            Advanced settings
          </span>
          <span className="text-xs font-normal muted">argv, env, retention, logs</span>
        </summary>
        <div className="mt-4 space-y-4 border-t border-base-300 pt-4">
          <div className="rounded-xl border border-base-300 bg-base-200/25 p-3">
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide muted">Execution</h3>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
              <div className="sm:col-span-2 lg:col-span-3">
                <label htmlFor={`${formId}-argv`} className={label}>Argv (one argument per line — overrides command)</label>
                <textarea
                  id={`${formId}-argv`}
                  className="mc-input"
                  value={(draft.argv ?? []).join('\n')}
                  disabled={disabled}
                  onChange={event =>
                    onChange({
                      argv: event.target.value
                        .split('\n')
                        .map(line => line.trim())
                        .filter(Boolean),
                      command: event.target.value.trim() ? undefined : draft.command,
                    })
                  }
                  placeholder={'/usr/bin/curl\n-fsS\nhttps://example.com'}
                />
              </div>
              <div>
                <label htmlFor={`${formId}-workdir`} className={label}>Working directory</label>
                <input id={`${formId}-workdir`} className={`${field} mono`} value={draft.working_dir ?? ''} disabled={disabled} onChange={event => onChange({ working_dir: event.target.value || undefined })} />
              </div>
              <div>
                <label htmlFor={`${formId}-shell`} className={label}>Shell</label>
                <input id={`${formId}-shell`} className={`${field} mono`} value={draft.shell ?? ''} disabled={disabled} onChange={event => onChange({ shell: event.target.value || undefined })} placeholder="sh" />
              </div>
              <div>
                <label htmlFor={`${formId}-signal`} className={label}>Stop signal</label>
                <input id={`${formId}-signal`} className={`${field} mono`} value={draft.stop_signal ?? ''} disabled={disabled} onChange={event => onChange({ stop_signal: event.target.value || undefined })} placeholder="TERM" />
              </div>
              <div>
                <label htmlFor={`${formId}-codes`} className={label}>Success codes</label>
                <input
                  id={`${formId}-codes`}
                  className={`${field} mono`}
                  value={(draft.success_codes ?? []).join(',')}
                  disabled={disabled}
                  onChange={event =>
                    onChange({
                      success_codes: event.target.value
                        .split(',')
                        .map(part => Number(part.trim()))
                        .filter(code => Number.isInteger(code)),
                    })
                  }
                  placeholder="0"
                />
              </div>
              {isJob && (
                <>
                  <div>
                    <label htmlFor={`${formId}-catchup`} className={label}>Catch up</label>
                    <select id={`${formId}-catchup`} className="mc-select" value={draft.catch_up ?? 'none'} disabled={disabled} onChange={event => onChange({ catch_up: event.target.value })}>
                      <option value="none">none</option>
                      <option value="latest">latest</option>
                    </select>
                  </div>
                  <label className="flex items-center gap-2.5 self-end pb-2 text-sm">
                    <span className="switch">
                      <input type="checkbox" checked={draft.run_on_start === true} disabled={disabled} onChange={event => onChange({ run_on_start: event.target.checked })} />
                      <span className="track" />
                    </span>
                    Run on daemon start
                  </label>
                </>
              )}
            </div>
          </div>

          <div className="rounded-xl border border-base-300 bg-base-200/25 p-3">
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide muted">Environment</h3>
            <div className="grid gap-3 sm:grid-cols-2">
              <div>
                <label htmlFor={`${formId}-envbase`} className={label}>Environment base</label>
                <select id={`${formId}-envbase`} className="mc-select" value={draft.env_base ?? 'clean'} disabled={disabled} onChange={event => onChange({ env_base: event.target.value })}>
                  <option value="clean">clean</option>
                  <option value="minimal">minimal</option>
                </select>
              </div>
              <div>
                <label htmlFor={`${formId}-envfile`} className={label}>Environment file</label>
                <input id={`${formId}-envfile`} className={`${field} mono`} value={draft.env_file ?? ''} disabled={disabled} onChange={event => onChange({ env_file: event.target.value || undefined })} placeholder="env.local" />
              </div>
              <div>
                <label htmlFor={`${formId}-env`} className={label}>Environment variables (KEY=VALUE per line)</label>
                <textarea
                  id={`${formId}-env`}
                  className="mc-input"
                  value={Object.entries(draft.env ?? {})
                    .map(([key, value]) => `${key}=${value}`)
                    .join('\n')}
                  disabled={disabled}
                  onChange={event => onChange({ env: parseKeyValue(event.target.value) })}
                  placeholder={'FOO=bar\nBAZ=qux'}
                />
              </div>
              <div>
                <label htmlFor={`${formId}-secenv`} className={label}>Secret environment (KEY=VALUE per line)</label>
                <textarea
                  id={`${formId}-secenv`}
                  className="mc-input"
                  value={Object.entries(draft.secret_env ?? {})
                    .map(([key, value]) => `${key}=${value}`)
                    .join('\n')}
                  disabled={disabled}
                  onChange={event => onChange({ secret_env: parseKeyValue(event.target.value) })}
                />
                <p className="field-help">Write-only values; the API never returns them.</p>
              </div>
            </div>
          </div>

          <div className="rounded-xl border border-base-300 bg-base-200/25 p-3">
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide muted">Alerts</h3>
            <fieldset>
              <legend className={label}>Alert channels</legend>
              {channelNames.length > 0 ? (
                <div className="grid gap-2 sm:grid-cols-2">
                  {channelNames.map(name => {
                    const configured = configuredChannels.find(channel => channel.name === name);
                    return (
                      <div key={name} className="flex items-center gap-2 rounded-lg border border-base-300 px-3 py-2 text-sm">
                        <input
                          id={`${formId}-alert-${name}`}
                          type="checkbox"
                          checked={selectedAlerts.includes(name)}
                          disabled={disabled}
                          onChange={event => onChange({
                            alerts: event.target.checked
                              ? [...selectedAlerts, name]
                              : selectedAlerts.filter(selected => selected !== name),
                          })}
                        />
                        <label htmlFor={`${formId}-alert-${name}`} className="min-w-0 break-all font-mono">{name}</label>
                        {configured ? <span className="ml-auto text-xs muted">{configured.type} · {configured.batch_window}s</span> : channels.isSuccess && (
                          <span className="ml-auto text-xs text-amber-300">Not configured</span>
                        )}
                        {configured && !readOnly && (
                          <button type="button" className="btn-sub" disabled={Boolean(testing)} onClick={() => {
                            setTesting(name);
                            setTestResult('');
                            void api.testAlertChannel(name)
                              .then(() => setTestResult(`${name}: test message sent`))
                              .catch(error => setTestResult(`${name}: ${errorText(error)}`))
                              .finally(() => setTesting(''));
                          }}>{testing === name ? 'Sending…' : 'Test'}</button>
                        )}
                      </div>
                    );
                  })}
                </div>
              ) : channels.isSuccess && (
                <p className="text-sm muted">No channels configured.</p>
              )}
              {testResult && <p role="status" className="mt-2 text-xs">{testResult}</p>}
              {channels.isPending && <p className="mt-2 text-xs muted">Loading channels…</p>}
              {channels.isError && <p role="alert" className="mt-2 text-xs text-red-300">Failed to load channels: {errorText(channels.error)}</p>}
              <p className="field-help">Failed and timed-out runs notify selected channels.</p>
            </fieldset>
          </div>

          <div className="rounded-xl border border-base-300 bg-base-200/25 p-3">
            <h3 className="mb-3 text-xs font-semibold uppercase tracking-wide muted">Retention & logs</h3>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <div>
                <label htmlFor={`${formId}-keepruns`} className={label}>Keep runs</label>
                <input id={`${formId}-keepruns`} type="number" min={0} className={field} value={draft.keep_runs ?? 0} disabled={disabled} onChange={event => onChange({ keep_runs: Number(event.target.value) || 0 })} />
              </div>
              <div>
                <label htmlFor={`${formId}-keepfor`} className={label}>Keep for (days)</label>
                <input id={`${formId}-keepfor`} type="number" min={0} step={1} className={field} value={draft.keep_for ?? ''} disabled={disabled} onChange={event => onChange({ keep_for: optionalNumber(event.target.value) })} placeholder="30" />
              </div>
              <div>
                <label htmlFor={`${formId}-logmax`} className={label}>Log max size (MiB)</label>
                <input id={`${formId}-logmax`} type="number" min={0} max={1048576} step={1} className={field} value={draft.log_max ?? ''} disabled={disabled} onChange={event => onChange({ log_max: optionalNumber(event.target.value) })} placeholder="100" />
                <p className="field-help">0 uses the default of 100 MiB.</p>
              </div>
              <div>
                <label htmlFor={`${formId}-logfull`} className={label}>When log is full</label>
                <select id={`${formId}-logfull`} className="mc-select" value={draft.log_on_full ?? 'drop_old'} disabled={disabled} onChange={event => onChange({ log_on_full: event.target.value })}>
                  <option value="drop_old">drop_old</option>
                  <option value="drop_new">drop_new</option>
                  <option value="kill">kill</option>
                </select>
              </div>
            </div>
          </div>
        </div>
      </details>
    </form>
  );
}

function optionalNumber(value: string): number | undefined {
  return value === '' ? undefined : Number(value);
}

function parseKeyValue(text: string): Record<string, string> | undefined {
  const result: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (!line || line.startsWith('#')) continue;
    const eq = line.indexOf('=');
    if (eq <= 0) continue;
    result[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return Object.keys(result).length > 0 ? result : undefined;
}
