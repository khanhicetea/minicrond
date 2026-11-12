import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api, errorText } from '../api';
import { alertChannelsQuery } from '../queries';
import { Icon } from './Icon';
import { Button, ChoiceCards, Field, FormSection, Segmented, SwitchRow, UnitInput } from './ui';
import type { Definition } from '../types';
import { buildCron, cronSelectValues, humanizeSchedule, nextFires, parseSchedule } from '../lib/cron';
import { TIMEZONES } from '../lib/timezones';
import { commandToArgv } from '../lib/command-argv';

const MINUTE_OPTIONS = ['*', '*/5', '*/10', '*/15', '*/30', ...Array.from({ length: 60 }, (_, i) => String(i))];
const HOUR_OPTIONS = ['*', '*/2', '*/4', '*/6', '*/12', ...Array.from({ length: 24 }, (_, i) => String(i))];
const DOM_OPTIONS = ['*', ...Array.from({ length: 31 }, (_, i) => String(i + 1))];
const MONTH_OPTIONS = ['*', ...Array.from({ length: 12 }, (_, i) => String(i + 1))];
const DOW_OPTIONS = ['*', '1-5', '0', '1', '2', '3', '4', '5', '6'];
const DOW_LABEL: Record<string, string> = { '*': 'any', '1-5': 'Mon–Fri', '0': 'Sun', '1': 'Mon', '2': 'Tue', '3': 'Wed', '4': 'Thu', '5': 'Fri', '6': 'Sat' };
const MONTH_LABEL = ['', 'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

const CRON_PRESETS: [string, string][] = [
  ['Every 5 min', '*/5 * * * *'],
  ['Hourly', '0 * * * *'],
  ['Daily 00:00', '0 0 * * *'],
  ['Weekdays 09:00', '0 9 * * 1-5'],
  ['Weekly (Mon)', '0 0 * * 1'],
  ['Monthly (1st)', '0 0 1 * *'],
];
const INTERVAL_PRESETS = ['1m', '5m', '15m', '30m', '1h', '6h', '24h'];

/** Sections in display order; the editor's side navigation links to these. */
export function formSections(kind: string): { id: string; title: string }[] {
  const worker = kind === 'worker';
  return [
    { id: 'basics', title: 'Basics' },
    { id: 'command', title: 'Command' },
    { id: worker ? 'lifecycle' : 'schedule', title: worker ? 'Lifecycle' : 'Schedule' },
    { id: 'environment', title: 'Environment' },
    { id: 'limits', title: worker ? 'Stopping' : 'Limits & failures' },
    { id: 'alerts', title: 'Alerts' },
    { id: 'retention', title: 'Retention & logs' },
  ];
}

/** Which section holds a validated field (for marking sections invalid). */
const FIELD_SECTION: Record<string, string> = {
  name: 'basics',
  command: 'command',
  schedule: 'schedule',
  timezone: 'schedule',
  catch_up: 'schedule',
  timeout: 'limits',
  on_overlap: 'limits',
  retries: 'limits',
  retry_delay: 'limits',
  success_codes: 'limits',
  restart_backoff_max: 'lifecycle',
  log_max: 'retention',
};

interface JobFormProps {
  /** Show the Job/Worker choice (blank "new definition" only). */
  showKindTabs: boolean;
  /** Lock the name field (editing an existing definition). */
  nameLocked: boolean;
  draft: Definition;
  readOnly: boolean;
  runAsEnabled: boolean;
  onChange: (patch: Partial<Definition>) => void;
  formId: string;
  /** Validation errors to show inline, keyed by field name. */
  errors?: Record<string, string>;
}

/**
 * Definition editor form. Controlled: the page owns the draft so the TOML
 * preview and validation can mirror it live. Field ids are `${formId}-${field}`
 * so validation messages can focus the offending control.
 */
export default function JobForm({ showKindTabs, nameLocked, draft, readOnly, runAsEnabled, onChange, formId, errors = {} }: JobFormProps) {
  const isJob = draft.kind !== 'worker';
  const disabled = readOnly;
  const id = (field: string) => `${formId}-${field}`;
  const sectionId = (section: string) => `${formId}-section-${section}`;
  const invalidSections = new Set(Object.keys(errors).map(field => FIELD_SECTION[field]).filter(Boolean));
  const steps = formSections(draft.kind);
  const step = (section: string) => steps.findIndex(item => item.id === section) + 1;

  // Ticking clock for the "next fires" preview.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(timer);
  }, []);

  return (
    <form id={formId} onSubmit={event => event.preventDefault()} className="editor-main" aria-label="Definition editor">
      <FormSection id={sectionId('basics')} step={step('basics')} title="Basics" description="What this definition is and how it is identified." invalid={invalidSections.has('basics')}>
        {showKindTabs && (
          <ChoiceCards<'job' | 'worker'>
            name={id('kind')}
            legend="Type"
            value={isJob ? 'job' : 'worker'}
            disabled={disabled}
            onChange={kind => onChange({ kind })}
            options={[
              { value: 'job', label: 'Job', icon: 'calendar', description: 'Runs a command on a schedule and exits.' },
              { value: 'worker', label: 'Worker', icon: 'terminal', description: 'Keeps a long-running process alive and restarts it when it exits.' },
            ]}
          />
        )}
        <div className="field-row">
          <Field id={id('name')} label="Name" error={errors.name} help={nameLocked ? 'The name cannot be changed after creation; clone to rename.' : 'Lowercase letters, digits, dots, dashes or underscores.'}>
            <input
              id={id('name')}
              className="mc-input mono"
              value={draft.name}
              autoFocus={!nameLocked && !disabled}
              readOnly={nameLocked || disabled}
              disabled={disabled}
              aria-invalid={Boolean(errors.name)}
              aria-describedby={errors.name ? `${id('name')}-error` : undefined}
              onChange={event => onChange({ name: event.target.value })}
              placeholder={isJob ? 'database-backup' : 'queue-consumer'}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          <Field id={id('description')} label="Description" optional help="Shown in the jobs list instead of the command.">
            <input
              id={id('description')}
              className="mc-input"
              value={draft.labels?.description ?? ''}
              disabled={disabled}
              onChange={event => {
                const labels = { ...(draft.labels ?? {}) };
                if (event.target.value) labels.description = event.target.value;
                else delete labels.description;
                onChange({ labels: Object.keys(labels).length ? labels : undefined });
              }}
              placeholder={isJob ? 'Nightly dump of the app database' : 'Consumes the orders queue'}
            />
          </Field>
        </div>
        <SwitchRow
          id={id('enabled')}
          label="Enabled"
          description={isJob ? 'Disabled jobs keep their history but never fire.' : 'Disabled workers are stopped and not restarted.'}
          checked={draft.enabled !== false}
          disabled={disabled}
          onChange={enabled => onChange({ enabled })}
        />
      </FormSection>

      <CommandSection draft={draft} disabled={disabled} id={id} sectionId={sectionId('command')} step={step('command')} errors={errors} runAsEnabled={runAsEnabled} onChange={onChange} invalid={invalidSections.has('command')} />

      {isJob ? (
        <ScheduleSection draft={draft} disabled={disabled} id={id} sectionId={sectionId('schedule')} step={step('schedule')} errors={errors} now={now} onChange={onChange} invalid={invalidSections.has('schedule')} />
      ) : (
        <FormSection id={sectionId('lifecycle')} step={step('lifecycle')} title="Lifecycle" description="When the worker starts, and how it is restarted after it exits." invalid={invalidSections.has('lifecycle')}>
          <SwitchRow id={id('autostart')} label="Start with the daemon" description="Off: the worker starts only when you start it." checked={draft.autostart !== false} disabled={disabled} onChange={autostart => onChange({ autostart })} />
          <ChoiceCards<string>
            name={id('restart')}
            legend="Restart policy"
            value={draft.restart ?? 'always'}
            disabled={disabled}
            columns={3}
            onChange={restart => onChange({ restart })}
            options={[
              { value: 'always', label: 'Always', description: 'Restart after any exit.' },
              { value: 'on-failure', label: 'On failure', description: 'Restart only after a failing exit.' },
              { value: 'never', label: 'Never', description: 'Leave it stopped once it exits.' },
            ]}
          />
          {draft.restart !== 'never' && (
            <div className="field-row">
              <Field id={id('restart_delay')} label="Restart delay" help="Wait before the first restart.">
                <UnitInput id={id('restart_delay')} unit="sec" type="number" min={1} step={1} value={draft.restart_delay ?? 5} disabled={disabled} onChange={event => onChange({ restart_delay: optionalNumber(event.target.value) })} />
              </Field>
              <Field id={id('max_restart_attempts')} label="Max restart attempts" help="Consecutive failures before giving up (1–1000).">
                <input id={id('max_restart_attempts')} type="number" min={1} max={1000} className="mc-input" value={draft.max_restart_attempts ?? 5} disabled={disabled} onChange={event => onChange({ max_restart_attempts: Number(event.target.value) || 0 })} />
              </Field>
              <Field id={id('restart_backoff_max')} label="Backoff ceiling" error={errors.restart_backoff_max} help="When set, the delay doubles up to this cap and the worker is never retired. 0 = off.">
                <UnitInput id={id('restart_backoff_max')} unit="sec" type="number" min={0} step={1} value={draft.restart_backoff_max ?? ''} disabled={disabled} onChange={event => onChange({ restart_backoff_max: optionalNumber(event.target.value) })} placeholder="0" />
              </Field>
              <Field id={id('healthy_after')} label="Healthy after" help="Run this long to reset the crash counter.">
                <UnitInput id={id('healthy_after')} unit="sec" type="number" min={1} step={1} value={draft.healthy_after ?? ''} disabled={disabled} onChange={event => onChange({ healthy_after: optionalNumber(event.target.value) })} placeholder="30" />
              </Field>
            </div>
          )}
        </FormSection>
      )}

      <FormSection
        id={sectionId('environment')}
        step={step('environment')}
        title="Environment"
        description="Variables the command sees."
        collapsible
        defaultOpen={Boolean(draft.env_file || Object.keys(draft.env ?? {}).length || Object.keys(draft.secret_env ?? {}).length)}
        summary={[draft.env_base ?? 'clean', draft.env_file ? 'env file' : '', `${Object.keys(draft.env ?? {}).length} vars`, Object.keys(draft.secret_env ?? {}).length ? `${Object.keys(draft.secret_env ?? {}).length} secret` : ''].filter(Boolean).join(' · ')}
      >
        <div className="field-row">
          <Field id={id('env_base')} label="Base environment" help={draft.env_base === 'inherit' ? "Starts from the daemon's environment (not its HOME, USER or LOGNAME)." : 'Minimal fixed set: PATH, TZ, HOME and the MINICROND_* variables.'}>
            <Segmented<string>
              label="Base environment"
              value={draft.env_base ?? 'clean'}
              onChange={env_base => !disabled && onChange({ env_base })}
              options={[
                { value: 'clean', label: 'Clean' },
                { value: 'inherit', label: 'Inherit' },
              ]}
            />
          </Field>
          <Field id={id('env_file')} label="Environment file" optional help="Absolute path to a KEY=VALUE file (up to 1 MiB).">
            <input id={id('env_file')} className="mc-input mono" value={draft.env_file ?? ''} disabled={disabled} onChange={event => onChange({ env_file: event.target.value || undefined })} placeholder="/etc/minicrond/app.env" />
          </Field>
        </div>
        <div className="field-row">
          <Field id={id('env')} label="Variables" help="One KEY=VALUE per line.">
            <KeyValueText id={id('env')} value={draft.env} disabled={disabled} onChange={env => onChange({ env })} placeholder={'APP_ENV=production\nLOG_LEVEL=info'} />
          </Field>
          <Field id={id('secret_env')} label="Secret variables" help="Values are env:NAME or file:/abs/path, resolved when the process starts.">
            <KeyValueText id={id('secret_env')} value={draft.secret_env} disabled={disabled} onChange={secret_env => onChange({ secret_env })} placeholder={'DB_PASSWORD=file:/run/secrets/db\nAPI_KEY=env:API_KEY'} />
          </Field>
        </div>
      </FormSection>

      <FormSection
        id={sectionId('limits')}
        step={step('limits')}
        title={isJob ? 'Limits & failures' : 'Stopping'}
        description={isJob ? 'How long a run may take, what counts as success, and what happens on failure.' : 'Runtime limit, how the worker is stopped, and which exit codes count as success.'}
        invalid={invalidSections.has('limits')}
      >
        <div className="field-row">
          <Field id={id('timeout')} label="Timeout" error={errors.timeout} help={isJob ? '0 disables the timeout.' : 'Stops the worker after this long; 0 = no limit.'}>
            <UnitInput id={id('timeout')} unit="sec" type="number" min={0} step={1} value={draft.timeout ?? ''} disabled={disabled} onChange={event => onChange({ timeout: optionalNumber(event.target.value) })} placeholder={isJob ? '1200' : '0'} />
          </Field>
          <Field id={id('grace')} label="Grace period" help="After the stop signal, before SIGKILL.">
            <UnitInput id={id('grace')} unit="sec" type="number" min={1} step={1} value={draft.grace ?? ''} disabled={disabled} onChange={event => onChange({ grace: optionalNumber(event.target.value) })} placeholder="10" />
          </Field>
          <Field id={id('stop_signal')} label="Stop signal">
            <select id={id('stop_signal')} className="mc-select mono" value={(draft.stop_signal ?? 'SIGTERM').replace(/^(?!SIG)/, 'SIG')} disabled={disabled} onChange={event => onChange({ stop_signal: event.target.value })}>
              {['SIGTERM', 'SIGINT', 'SIGHUP', 'SIGQUIT', 'SIGUSR1', 'SIGUSR2', 'SIGKILL'].map(signal => (
                <option key={signal} value={signal}>
                  {signal}
                </option>
              ))}
            </select>
          </Field>
          <Field id={id('success_codes')} label="Success exit codes" error={errors.success_codes} help="Comma separated.">
            <SuccessCodes id={id('success_codes')} value={draft.success_codes} disabled={disabled} onChange={success_codes => onChange({ success_codes })} />
          </Field>
        </div>

        {isJob && (
          <>
            <div className="subgroup">
              <div className="field-row">
                <Field id={id('retries')} label="Retries after failure" error={errors.retries} help="Timeouts and stopped runs are not retried.">
                  <input id={id('retries')} type="number" min={0} max={1000} step={1} className="mc-input" value={draft.retries ?? 0} disabled={disabled} onChange={event => onChange({ retries: optionalNumber(event.target.value) })} />
                </Field>
                <Field id={id('retry_delay')} label="Retry delay" error={errors.retry_delay}>
                  <UnitInput id={id('retry_delay')} unit="sec" type="number" min={0} max={86400} step={1} value={draft.retry_delay ?? 5} disabled={disabled || !(draft.retries ?? 0)} onChange={event => onChange({ retry_delay: optionalNumber(event.target.value) })} />
                </Field>
              </div>
            </div>
            <ChoiceCards<string>
              name={id('on_overlap')}
              legend="If the previous run is still active"
              value={draft.on_overlap ?? 'skip'}
              disabled={disabled}
              onChange={on_overlap => onChange({ on_overlap })}
              options={[
                { value: 'skip', label: 'Skip the new run', description: 'At most one run at a time.' },
                { value: 'parallel', label: 'Run in parallel', description: 'Start the new run anyway.' },
              ]}
            />
            <ChoiceCards<string>
              name={id('on_history_unavailable')}
              legend="If run history cannot be written"
              value={draft.on_history_unavailable || 'run'}
              disabled={disabled}
              onChange={on_history_unavailable => onChange({ on_history_unavailable })}
              options={[
                { value: 'run', label: 'Run anyway', description: 'Keeps the schedule; a daemon crash may repeat a run.' },
                { value: 'skip', label: 'Skip the run', description: 'For work that must never run twice, such as billing or email.' },
              ]}
            />
          </>
        )}
      </FormSection>

      <AlertsSection draft={draft} disabled={disabled} id={id} sectionId={sectionId('alerts')} step={step('alerts')} onChange={onChange} />

      <FormSection
        id={sectionId('retention')}
        step={step('retention')}
        title="Retention & logs"
        description="How much history and output to keep."
        collapsible
        defaultOpen={false}
        invalid={invalidSections.has('retention')}
        summary={[draft.keep_runs ? `${draft.keep_runs} runs` : 'default runs', draft.keep_for ? `${draft.keep_for} days` : '', draft.log_max ? `${draft.log_max} MiB log` : '100 MiB log', draft.log_on_full ?? 'drop_old'].filter(Boolean).join(' · ')}
      >
        <div className="field-row">
          <Field id={id('keep_runs')} label="Keep runs" help="0 uses the daemon default.">
            <input id={id('keep_runs')} type="number" min={0} className="mc-input" value={draft.keep_runs ?? 0} disabled={disabled} onChange={event => onChange({ keep_runs: Number(event.target.value) || 0 })} />
          </Field>
          <Field id={id('keep_for')} label="Keep for" help="0 uses the daemon default.">
            <UnitInput id={id('keep_for')} unit="days" type="number" min={0} step={1} value={draft.keep_for ?? ''} disabled={disabled} onChange={event => onChange({ keep_for: optionalNumber(event.target.value) })} placeholder="7" />
          </Field>
          <Field id={id('log_max')} label="Log buffer limit" error={errors.log_max} help="Per-run buffer before archival; 0 uses 100 MiB.">
            <UnitInput id={id('log_max')} unit="MiB" type="number" min={0} max={1048576} step={1} value={draft.log_max ?? ''} disabled={disabled} onChange={event => onChange({ log_max: optionalNumber(event.target.value) })} placeholder="100" />
          </Field>
          <Field id={id('log_on_full')} label="When the log is full">
            <select id={id('log_on_full')} className="mc-select" value={draft.log_on_full ?? 'drop_old'} disabled={disabled} onChange={event => onChange({ log_on_full: event.target.value })}>
              <option value="drop_old">Drop oldest output</option>
              <option value="drop_new">Drop new output</option>
            </select>
          </Field>
        </div>
      </FormSection>
    </form>
  );
}

interface SectionProps {
  draft: Definition;
  disabled: boolean;
  id: (field: string) => string;
  sectionId: string;
  step: number;
  onChange: (patch: Partial<Definition>) => void;
}

function CommandSection({ draft, disabled, id, sectionId, step, errors, runAsEnabled, onChange, invalid }: SectionProps & { errors: Record<string, string>; runAsEnabled: boolean; invalid: boolean }) {
  const [mode, setModeState] = useState<'shell' | 'argv'>(draft.argv?.length ? 'argv' : 'shell');
  // Follow outside changes (kind switch, "Switch to direct" suggestion).
  if (mode === 'shell' && draft.argv?.length) setModeState('argv');
  if (mode === 'argv' && draft.command) setModeState('shell');
  const suggestedArgv = !disabled && mode === 'shell' && (!draft.shell || draft.shell === '/bin/sh') ? commandToArgv(draft.command ?? '') : null;

  const setMode = (next: 'shell' | 'argv') => {
    if (next === mode || disabled) return;
    setModeState(next);
    if (next === 'argv') onChange({ argv: commandToArgv(draft.command ?? '') ?? undefined, command: undefined });
    else onChange({ command: (draft.argv ?? []).join(' ') || undefined, argv: undefined });
  };

  return (
    <FormSection id={sectionId} step={step} title="Command" description="What runs, as whom, and where." invalid={invalid}>
      <Field id={id('mode')} label="Run with">
        <Segmented<'shell' | 'argv'>
          label="Run with"
          value={mode}
          onChange={setMode}
          options={[
            { value: 'shell', label: 'Shell command' },
            { value: 'argv', label: 'Direct (argv)' },
          ]}
        />
      </Field>
      {mode === 'shell' ? (
        <>
          <Field id={id('command')} label="Command" error={errors.command} help={`Runs as ${draft.shell || '/bin/sh'} -c "<command>".`}>
            <input
              id={id('command')}
              className="mc-input mono"
              value={draft.command ?? ''}
              disabled={disabled}
              aria-invalid={Boolean(errors.command)}
              onChange={event => onChange({ command: event.target.value, argv: undefined })}
              placeholder={draft.kind === 'worker' ? 'node consumer.js --prefetch 20' : 'pg_dump --format=custom appdb'}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          {suggestedArgv && (
            <div className="subgroup !flex flex-wrap items-center !gap-2 text-xs">
              <Icon name="zap" size={13} className="text-link" />
              <span className="flex-1">No shell features used — it can run directly, without a shell.</span>
              <Button size="sm" onClick={() => onChange({ command: undefined, argv: suggestedArgv })}>
                Switch to direct
              </Button>
            </div>
          )}
        </>
      ) : (
        <Field id={id('command')} label="Arguments" error={errors.command} help="One argument per line; the first is the program. No shell expansion.">
          <LinesText id={id('command')} value={draft.argv} disabled={disabled} onChange={argv => onChange({ argv, command: undefined })} placeholder={'/usr/bin/curl\n-fsS\nhttps://example.com/health'} />
        </Field>
      )}
      <div className="field-row">
        <Field id={id('run_as')} label="Run as" optional help={runAsEnabled ? 'user, uid, or user:group.' : 'Available only when the daemon runs as root.'}>
          <input id={id('run_as')} className="mc-input mono" value={draft.run_as ?? ''} disabled={disabled || !runAsEnabled} onChange={event => onChange({ run_as: event.target.value || undefined })} placeholder="backup" />
        </Field>
        <Field id={id('working_dir')} label="Working directory" optional help="Absolute path; defaults to the daemon's.">
          <input id={id('working_dir')} className="mc-input mono" value={draft.working_dir ?? ''} disabled={disabled} onChange={event => onChange({ working_dir: event.target.value || undefined })} placeholder="/srv/app" />
        </Field>
        {mode === 'shell' && (
          <Field id={id('shell')} label="Shell" optional>
            <input id={id('shell')} className="mc-input mono" value={draft.shell ?? ''} disabled={disabled} onChange={event => onChange({ shell: event.target.value || undefined })} placeholder="/bin/sh" />
          </Field>
        )}
      </div>
    </FormSection>
  );
}

function ScheduleSection({ draft, disabled, id, sectionId, step, errors, now, onChange, invalid }: SectionProps & { errors: Record<string, string>; now: number; invalid: boolean }) {
  const schedule = draft.schedule ?? '';
  const type: 'cron' | 'every' = schedule.trim().toLowerCase().startsWith('@every') ? 'every' : 'cron';
  const tz = draft.timezone || 'UTC';
  const valid = Boolean(parseSchedule(schedule));
  const fires = valid ? nextFires(schedule, 5, now, tz) : [];
  const cronParts = cronSelectValues(schedule);
  const setCronPart = (index: number, value: string) => {
    const parts = [...cronParts];
    parts[index] = value;
    onChange({ schedule: buildCron(parts[0], parts[1], parts[2], parts[3], parts[4]) });
  };
  const presets = type === 'cron' ? CRON_PRESETS : INTERVAL_PRESETS.map(value => [`Every ${value}`, `@every ${value}`] as [string, string]);

  return (
    <FormSection id={sectionId} step={step} title="Schedule" description="When the job fires." invalid={invalid}>
      <Field id={id('schedule-type')} label="Schedule type">
        <Segmented<'cron' | 'every'>
          label="Schedule type"
          value={type}
          onChange={next => !disabled && next !== type && onChange({ schedule: next === 'every' ? '@every 15m' : '0 0 * * *' })}
          options={[
            { value: 'cron', label: 'Calendar (cron)' },
            { value: 'every', label: 'Fixed interval' },
          ]}
        />
      </Field>

      <div className="flex flex-wrap gap-1.5" role="group" aria-label="Schedule presets">
        {presets.map(([label, value]) => (
          <button key={value} type="button" className={`pill ${schedule.trim() === value ? 'active' : ''}`} aria-pressed={schedule.trim() === value} disabled={disabled} onClick={() => onChange({ schedule: value })}>
            {label}
          </button>
        ))}
      </div>

      <div className="field-row">
        <Field id={id('schedule')} label={type === 'every' ? 'Interval' : 'Cron expression'} error={errors.schedule} help={valid ? humanizeSchedule(schedule) : undefined}>
          <input
            id={id('schedule')}
            className="mc-input mono"
            value={schedule}
            disabled={disabled}
            aria-invalid={Boolean(errors.schedule)}
            onChange={event => onChange({ schedule: event.target.value })}
            placeholder={type === 'every' ? '@every 15m' : '0 2 * * *'}
            spellCheck={false}
          />
        </Field>
        <Field id={id('timezone')} label="Timezone" error={errors.timezone}>
          <select id={id('timezone')} className="mc-select mono" value={draft.timezone ?? 'UTC'} disabled={disabled} onChange={event => onChange({ timezone: event.target.value })}>
            {TIMEZONES.map(zone => (
              <option key={zone} value={zone}>
                {zone}
              </option>
            ))}
          </select>
        </Field>
      </div>

      <div className="field-row">
        {type === 'cron' && (
          <div className="subgroup">
            <span className="text-xs font-semibold muted">Build it field by field</span>
            <div className="grid grid-cols-5 gap-2">
              {(
                [
                  ['Minute', MINUTE_OPTIONS, 0],
                  ['Hour', HOUR_OPTIONS, 1],
                  ['Day', DOM_OPTIONS, 2],
                  ['Month', MONTH_OPTIONS, 3],
                  ['Weekday', DOW_OPTIONS, 4],
                ] as [string, string[], number][]
              ).map(([name, options, index]) => (
                <div key={name} className="min-w-0">
                  <label htmlFor={id(`cron-${index}`)} className="mb-1 block text-[0.6875rem] font-medium faint">
                    {name}
                  </label>
                  <select id={id(`cron-${index}`)} className="mc-select mono !min-h-[2rem] !px-2 !pr-6 !text-xs" value={cronParts[index] ?? '*'} disabled={disabled} onChange={event => setCronPart(index, event.target.value)}>
                    {options.map(option => (
                      <option key={option} value={option}>
                        {index === 4 ? (DOW_LABEL[option] ?? option) : index === 3 && option !== '*' ? MONTH_LABEL[Number(option)] : option}
                      </option>
                    ))}
                  </select>
                </div>
              ))}
            </div>
          </div>
        )}
        <div className="subgroup">
          <span className="text-xs font-semibold muted">Next fires · {tz}</span>
          {fires.length > 0 ? (
            <ol className="next-fires">
              {fires.map((fire, index) => (
                <li key={fire}>
                  <Icon name="clock" size={12} className="faint shrink-0" />
                  {new Date(fire).toLocaleString(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })}
                  {index === 0 && <span className="chip chip-info !px-1.5 !py-0 !text-[0.625rem]">next</span>}
                </li>
              ))}
            </ol>
          ) : (
            <p className="text-xs faint">Enter a valid schedule to preview when it fires.</p>
          )}
        </div>
      </div>

      <ChoiceCards<string>
        name={id('catch_up')}
        legend="After downtime (missed fires)"
        value={draft.catch_up ?? 'none'}
        disabled={disabled}
        onChange={catch_up => onChange({ catch_up })}
        options={[
          { value: 'none', label: 'Skip missed fires', description: 'Wait for the next scheduled time.' },
          { value: 'latest', label: 'Run the latest once', description: 'Fire the most recent miss; earlier ones are marked missed.' },
        ]}
      />
    </FormSection>
  );
}

function AlertsSection({ draft, disabled, id, sectionId, step, onChange }: SectionProps) {
  const channels = useQuery(alertChannelsQuery());
  const [testing, setTesting] = useState('');
  const [testResult, setTestResult] = useState('');
  const selected = draft.alerts ?? [];
  const configured = channels.data ?? [];
  // Keep saved names that are no longer configured available for removal.
  const names = [...new Set([...configured.map(channel => channel.name), ...selected])];
  return (
    <FormSection
      id={sectionId}
      step={step}
      title="Alerts"
      description="Failed and timed-out runs notify the selected channels."
      collapsible
      defaultOpen={selected.length > 0}
      summary={selected.length ? selected.join(', ') : 'none'}
    >
      {names.length > 0 ? (
        <fieldset className="m-0 grid gap-2 border-0 p-0">
          <legend className="sr-only">Alert channels</legend>
          {names.map(name => {
            const channel = configured.find(item => item.name === name);
            return (
              <div key={name} className="switch-row !cursor-default">
                <input
                  id={id(`alert-${name}`)}
                  type="checkbox"
                  className="h-4 w-4 accent-[var(--mc-accent)]"
                  checked={selected.includes(name)}
                  disabled={disabled}
                  onChange={event => onChange({ alerts: event.target.checked ? [...selected, name] : selected.filter(item => item !== name) })}
                />
                <label htmlFor={id(`alert-${name}`)} className="min-w-0 flex-1 cursor-pointer">
                  <span className="block break-all mono text-sm">{name}</span>
                  {channel ? (
                    <span className="text-xs faint">
                      {channel.type} · batches every {channel.batch_window}s
                    </span>
                  ) : (
                    channels.isSuccess && <span className="text-xs text-warn-fg">Not configured any more</span>
                  )}
                </label>
                {channel && !disabled && (
                  <Button
                    size="sm"
                    busy={testing === name}
                    disabled={Boolean(testing)}
                    onClick={() => {
                      setTesting(name);
                      setTestResult('');
                      void api
                        .testAlertChannel(name)
                        .then(() => setTestResult(`${name}: test message sent`))
                        .catch(error => setTestResult(`${name}: ${errorText(error)}`))
                        .finally(() => setTesting(''));
                    }}
                  >
                    Send test
                  </Button>
                )}
              </div>
            );
          })}
        </fieldset>
      ) : (
        channels.isSuccess && <p className="text-sm muted">No alert channels yet. Add one under Settings → Alert channels.</p>
      )}
      {testResult && (
        <p role="status" className="text-xs">
          {testResult}
        </p>
      )}
      {channels.isPending && <p className="text-xs muted">Loading channels…</p>}
      {channels.isError && (
        <p role="alert" className="text-xs text-danger-fg">
          Failed to load channels: {errorText(channels.error)}
        </p>
      )}
    </FormSection>
  );
}

/** Comma-separated exit codes; keeps the raw text while typing. */
function SuccessCodes({ id, value, disabled, onChange }: { id: string; value?: number[]; disabled: boolean; onChange: (codes: number[]) => void }) {
  const [text, setText] = useRawText((value ?? []).join(', '), raw => codesOf(raw).join(', '));
  return (
    <input
      id={id}
      className="mc-input mono"
      value={text}
      disabled={disabled}
      placeholder="0"
      onChange={event => {
        setText(event.target.value);
        onChange(codesOf(event.target.value));
      }}
    />
  );
}

/** One value per line (argv); keeps blank and partial lines while typing. */
function LinesText({ id, value, disabled, onChange, placeholder }: { id: string; value?: string[]; disabled: boolean; onChange: (lines: string[] | undefined) => void; placeholder?: string }) {
  const [text, setText] = useRawText((value ?? []).filter(Boolean).join('\n'), raw => linesOf(raw).join('\n'));
  return (
    <textarea
      id={id}
      className="mc-input"
      rows={Math.min(8, Math.max(3, text.split('\n').length + 1))}
      value={text}
      disabled={disabled}
      placeholder={placeholder}
      spellCheck={false}
      onChange={event => {
        setText(event.target.value);
        const lines = linesOf(event.target.value);
        onChange(lines.length ? lines : undefined);
      }}
    />
  );
}

/** KEY=VALUE per line; keeps lines without "=" yet while typing. */
function KeyValueText({ id, value, disabled, onChange, placeholder }: { id: string; value?: Record<string, string>; disabled: boolean; onChange: (env: Record<string, string> | undefined) => void; placeholder?: string }) {
  const [text, setText] = useRawText(kvText(value), raw => kvText(parseKeyValue(raw)));
  return (
    <textarea
      id={id}
      className="mc-input"
      rows={Math.min(8, Math.max(3, text.split('\n').length + 1))}
      value={text}
      disabled={disabled}
      placeholder={placeholder}
      spellCheck={false}
      onChange={event => {
        setText(event.target.value);
        onChange(parseKeyValue(event.target.value));
      }}
    />
  );
}

/**
 * Raw text for a field whose stored value is parsed from it. The typed text is
 * kept while it still parses to the stored value; when the value changes from
 * outside (kind switch, conversion), the text is re-derived from it.
 */
function useRawText(derived: string, canonical: (text: string) => string): [string, (text: string) => void] {
  const [text, setText] = useState(derived);
  if (canonical(text) !== derived) {
    setText(derived);
    return [derived, setText];
  }
  return [text, setText];
}

const linesOf = (text: string) => text.split('\n').map(line => line.trim()).filter(Boolean);
const codesOf = (text: string) => text.split(',').map(part => part.trim()).filter(Boolean).map(Number).filter(code => Number.isInteger(code));
const kvText = (value?: Record<string, string>) => Object.entries(value ?? {}).map(([key, item]) => `${key}=${item}`).join('\n');

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
