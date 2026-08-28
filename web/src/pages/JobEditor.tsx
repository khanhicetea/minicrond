import { useEffect, useMemo, useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import JobForm, { emptyDefinition } from '../components/JobForm';
import RunsTable from '../components/RunsTable';
import { errorText } from '../api';
import { jobQuery, runsQuery, useDeleteJob, useSaveJob } from '../queries';
import { definitionToToml, highlightToml } from '../lib/toml';
import { validateDefinition, type DefinitionIssue } from '../lib/validate';
import { jobPath } from '../lib/routes';
import type { Definition } from '../types';

const FORM_ID = 'job-editor-form';

interface JobEditorProps {
  params?: { name?: string };
}

/** Definition editor used for both "New definition" and editing an existing one. */
export default function JobEditor({ params }: JobEditorProps) {
  const routeName = params?.name;
  const existingName = routeName === 'new' ? undefined : routeName;
  const copyFrom = new URLSearchParams(useSearch()).get('from') ?? undefined;
  const [, navigate] = useLocation();
  const creating = !existingName;

  const detail = useQuery({ ...jobQuery(existingName ?? ''), enabled: Boolean(existingName) });
  const source = useQuery({ ...jobQuery(copyFrom ?? ''), enabled: Boolean(copyFrom) });

  const save = useSaveJob();
  const remove = useDeleteJob();

  const [draft, setDraft] = useState<Definition>(() => emptyDefinition('job'));
  const [loaded, setLoaded] = useState(false);
  const [tab, setTab] = useState<'form' | 'toml'>('toml');
  const [serverError, setServerError] = useState('');
  const [savedFlash, setSavedFlash] = useState(false);

  // Initialize the draft once definition data arrives.
  useEffect(() => {
    if (creating && copyFrom && source.data) {
      const { definition } = source.data;
      setDraft({ ...definition, name: `${definition.name}-copy`, revision: undefined, definition_id: undefined, authority: undefined, source_file: undefined });
      setLoaded(true);
    } else if (!creating && detail.data) {
      setDraft({ ...detail.data.definition });
      setLoaded(true);
    } else if (creating && !copyFrom) {
      setLoaded(true);
    }
  }, [creating, copyFrom, source.data, detail.data]);

  const patch = (changes: Partial<Definition>) => setDraft(previous => ({ ...previous, ...changes }));

  const { text: tomlText, lineOf } = useMemo(() => definitionToToml(draft), [draft]);
  const issues = useMemo(() => validateDefinition(draft, lineOf), [draft, lineOf]);
  const errors = issues.filter(issue => issue.severity === 'error');
  const hints = issues.filter(issue => issue.severity === 'hint');
  const fileManaged = draft.authority === 'file' && !creating;

  const submit = () => {
    if (errors.length > 0) {
      setTab('toml');
      return;
    }
    setServerError('');
    // Strip server-side metadata; keep operator fields only.
    const payload: Definition = { ...draft };
    delete payload.definition_id;
    delete payload.authority;
    delete payload.source_file;
    if (!payload.command) delete payload.command;
    if (!payload.argv || payload.argv.length === 0) delete payload.argv;
    save.mutate(
      creating ? { definition: payload } : { name: existingName, definition: payload, revision: draft.revision },
      {
        onSuccess: saved => {
          if (creating) void navigate(jobPath(saved.name));
          else {
            setSavedFlash(true);
            setTimeout(() => setSavedFlash(false), 2500);
          }
        },
        onError: error => setServerError(errorText(error)),
      },
    );
  };

  if (!creating && detail.isPending) {
    return <div className="skeleton h-64 w-full" />;
  }
  if (!creating && detail.isError) {
    return (
      <div role="alert" className="rounded-lg border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-300">
        Failed to load {existingName}: {errorText(detail.error)}
      </div>
    );
  }
  if (!loaded) return <div className="skeleton h-64 w-full" />;

  const isWorker = draft.kind === 'worker';

  return (
    <div className="space-y-4">
      {/* Breadcrumb */}
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link href="/jobs">Jobs &amp; Workers</Link>
        <Icon name="chevron-right" size={12} className="faint" />
        <span>{creating ? (copyFrom ? `Duplicate of ${copyFrom}` : 'New definition') : draft.name}</span>
      </nav>

      {/* Title row */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3">
          <h1 className="text-[1.45rem] font-bold tracking-tight">
            {creating ? (isWorker ? 'New worker' : 'New job') : <span className="font-mono">{draft.name}</span>}
          </h1>
          <span className={`chip ${fileManaged ? 'chip-warn' : 'chip-info'}`}>
            <Icon name={fileManaged ? 'file-text' : 'database'} size={12} />
            {fileManaged ? `file: ${draft.source_file}` : 'DB authority'}
          </span>
          {savedFlash && <span className="chip chip-success">Saved</span>}
        </div>
        <div className="flex items-center gap-2">
          <Link href={creating || !existingName ? '/jobs' : jobPath(existingName)} className="btn-sub">
            Cancel
          </Link>
          {!fileManaged && (
            <button type="button" className="btn-primary-x" onClick={submit} disabled={save.isPending}>
              {save.isPending && <Icon name="loader" size={14} className="spin" />}
              {creating ? 'Review changes' : 'Save changes'}
            </button>
          )}
        </div>
      </div>

      {fileManaged && (
        <div role="alert" className="panel flex items-center gap-2.5 border-amber-500/40 bg-amber-500/5 px-4 py-3 text-sm">
          <Icon name="alert-triangle" size={16} className="text-amber-400 shrink-0" />
          <span>
            This definition is managed by <code className="font-mono">{draft.source_file}</code>. Edit that file and run{' '}
            <code className="font-mono">minicron reload</code>; API edits are rejected.
          </span>
        </div>
      )}
      {serverError && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-3 text-sm text-red-300">
          {serverError}
        </div>
      )}
      {!fileManaged && errors.length > 0 && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-2.5 text-sm text-red-300">
          Fix {errors.length} validation {errors.length === 1 ? 'issue' : 'issues'} before saving — see the panel on the right.
        </div>
      )}

      {/* Two-column editor: form 8/12, live TOML + validation 4/12 on desktop. */}
      <div className="grid gap-4 xl:grid-cols-12">
        <div className="min-w-0 xl:col-span-8">
          <JobForm
            showKindTabs={creating && !copyFrom}
            nameLocked={!creating || fileManaged}
            draft={draft}
            readOnly={fileManaged}
            onChange={patch}
            formId={FORM_ID}
          />
        </div>

        <div className="space-y-4 min-w-0 xl:col-span-4">
          <ValidationCard errors={errors} hints={hints} />

          <section className="panel flex min-h-[24rem] flex-col overflow-hidden">
            <div className="flex items-center justify-between border-b border-base-300 px-3 py-2">
              <div className="tab-seg">
                <button type="button" className={tab === 'form' ? 'active' : ''} onClick={() => setTab('form')}>
                  Form
                </button>
                <button type="button" className={tab === 'toml' ? 'active' : ''} onClick={() => setTab('toml')}>
                  TOML source
                </button>
              </div>
              <button
                type="button"
                className="btn-icon"
                title="Copy TOML to clipboard"
                onClick={() => void navigator.clipboard.writeText(tomlText)}
              >
                <Icon name="copy" size={14} />
              </button>
            </div>
            <div className="min-h-0 flex-1 overflow-auto">
              {tab === 'toml' ? (
                <div className="toml-view py-2">
                  {tomlText.split('\n').map((line, index) => (
                    <div key={index} className="toml-line">
                      <span className="toml-num">{index + 1}</span>
                      <span className="whitespace-pre">
                        {highlightToml(line).map((segment, segIndex) => (
                          <span key={segIndex} className={segment.cls}>
                            {segment.text}
                          </span>
                        ))}
                      </span>
                    </div>
                  ))}
                </div>
              ) : (
                <DefinitionSummary draft={draft} />
              )}
            </div>
          </section>

        </div>
      </div>

      {/* Edit mode: run history + danger zone */}
      {!creating && existingName && (
        <EditExtras name={existingName} fileManaged={fileManaged} onDelete={() => remove.mutate(existingName, { onSuccess: () => navigate('/jobs') })} deleting={remove.isPending} />
      )}
    </div>
  );
}


function ValidationCard({ errors, hints }: { errors: DefinitionIssue[]; hints: DefinitionIssue[] }) {
  const ok = errors.length === 0;
  const issues = ok ? hints : [...errors, ...hints];
  return (
    <section
      role={ok && hints.length === 0 ? 'status' : 'alert'}
      className={`panel overflow-hidden ${ok ? 'border-green-500/35 bg-green-500/5' : errors.length > 0 ? 'border-red-500/35 bg-red-500/5' : 'border-amber-500/35 bg-amber-500/5'}`}
    >
      <div className="flex items-center gap-2 border-b border-base-300 px-3 py-2">
        <span className={`flex h-6 w-6 shrink-0 items-center justify-center rounded-full ${ok ? 'bg-green-500/15 text-green-400' : errors.length > 0 ? 'bg-red-500/15 text-red-400' : 'bg-amber-500/15 text-amber-400'}`}>
          <Icon name={ok ? 'check' : errors.length > 0 ? 'alert-circle' : 'alert-triangle'} size={13} />
        </span>
        <div className="min-w-0">
          <p className={`text-sm font-semibold ${ok ? 'text-green-400' : errors.length > 0 ? 'text-red-400' : 'text-amber-400'}`}>Configuration check</p>
          <p className="text-xs muted">{ok ? (hints.length ? `${hints.length} suggestion${hints.length === 1 ? '' : 's'}` : 'No issues found') : `${errors.length} error${errors.length === 1 ? '' : 's'}${hints.length ? ` · ${hints.length} hint${hints.length === 1 ? '' : 's'}` : ''}`}</p>
        </div>
      </div>
      {issues.length > 0 && (
        <div className="divide-y divide-base-300">
          {issues.map(issue => {
            const error = issue.severity === 'error';
            return (
              <div key={`${issue.field}-${issue.message}`} className="grid grid-cols-[auto_1fr] gap-x-2 px-3 py-2">
                <Icon name={error ? 'alert-circle' : 'alert-triangle'} size={13} className={error ? 'mt-0.5 text-red-400' : 'mt-0.5 text-amber-400'} />
                <div className="min-w-0">
                  <p className={`flex items-baseline justify-between gap-2 text-xs font-semibold ${error ? 'text-red-400' : 'text-amber-400'}`}>
                    <span className="truncate">{issue.title}</span>
                    {issue.line && <span className="num shrink-0 opacity-80">Line {issue.line}</span>}
                  </p>
                  <p className="text-xs muted">{issue.message}</p>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}

/** Run history + danger zone shown under the editor for existing definitions. */
function EditExtras({ name, fileManaged, onDelete, deleting }: { name: string; fileManaged: boolean; onDelete: () => void; deleting: boolean }) {
  const detail = useQuery(jobQuery(name));
  const history = useQuery(runsQuery(name, 50));
  const { hash, active_runs } = detail.data ?? {};
  return (
    <div className="space-y-4 pt-2">
      <section>
        <div className="mb-3 flex items-center justify-between">
          <h2 className="panel-title">Run history</h2>
          <span className="num faint">
            {active_runs ?? 0} active · hash {hash ? hash.slice(0, 12) : '—'}
          </span>
        </div>
        {history.isPending ? <div className="skeleton h-40 w-full" /> : <RunsTable runs={history.data ?? []} compact />}
      </section>

      <section>
        <h2 className="mb-2 text-sm font-semibold text-red-400">Danger zone</h2>
        <div className="panel flex flex-wrap items-center justify-between gap-3 border-red-500/30 px-4 py-3">
          <p className="text-sm muted">
            Removes the {fileManaged ? 'definition entry' : 'DB-authority definition'}. Past runs and logs are kept until retention cleans them.
          </p>
          <button type="button" className="btn-danger-x" disabled={fileManaged || deleting} onClick={onDelete}>
            <Icon name="trash" size={14} />
            Delete definition
          </button>
        </div>
      </section>
    </div>
  );
}

function DefinitionSummary({ draft }: { draft: Definition }) {
  const rows: [string, string][] = [
    ['Kind', draft.kind],
    ['Command', draft.command ?? ((draft.argv ?? []).join(' ') || '—')],
    ['Schedule', draft.schedule ?? (draft.kind === 'worker' ? 'continuous' : '—')],
    ['Timezone', draft.timezone ?? 'UTC'],
    ['Run as', draft.run_as ?? '—'],
    ['Timeout', draft.timeout ?? '—'],
    ['Grace', draft.grace ?? '—'],
    ['Overlap', draft.on_overlap ?? '—'],
    ['Catch up', draft.catch_up ?? '—'],
    ['Run on start', draft.run_on_start ? 'yes' : 'no'],
    ['Env base', draft.env_base ?? 'clean'],
    ['Env file', draft.env_file ?? '—'],
    ['Keep runs', String(draft.keep_runs ?? 0)],
    ['Keep for', draft.keep_for ?? '—'],
    ['Labels', Object.entries(draft.labels ?? {}).map(([k, v]) => `${k}=${v}`).join(', ') || '—'],
  ];
  return (
    <dl className="grid grid-cols-[9rem_1fr] gap-y-2.5 p-4 text-sm">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="muted">{label}</dt>
          <dd className="break-words font-mono text-[0.8rem]">{value}</dd>
        </div>
      ))}
    </dl>
  );
}
