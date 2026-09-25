import { useEffect, useRef, useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import JobForm, { emptyDefinition } from '../components/JobForm';
import RunsTable from '../components/RunsTable';
import { errorText } from '../api';
import { daemonQuery, jobQuery, runsQuery, useDeleteJob, useSaveJob } from '../queries';
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
  const editorKey = creating ? `new:${copyFrom ?? ''}` : `edit:${existingName}`;

  const detail = useQuery({ ...jobQuery(existingName ?? ''), enabled: Boolean(existingName) });
  const source = useQuery({ ...jobQuery(copyFrom ?? ''), enabled: Boolean(copyFrom) });
  const daemon = useQuery(daemonQuery());

  const save = useSaveJob();
  const remove = useDeleteJob();

  const [draft, setDraft] = useState<Definition>(() => emptyDefinition('job'));
  const [loadedKey, setLoadedKey] = useState('');
  const initializedKey = useRef('');
  const [dirty, setDirty] = useState(false);
  const [tab, setTab] = useState<'form' | 'toml'>('toml');
  const [serverError, setServerError] = useState('');
  const [savedFlash, setSavedFlash] = useState(false);

  // Query data can change after focus or invalidation. Never replace an open draft.
  useEffect(() => {
    if (initializedKey.current === editorKey) return;
    if (creating && copyFrom && source.data) {
      const { definition } = source.data;
      setDraft({ ...definition, name: `${definition.name}-copy`, revision: undefined, definition_id: undefined });
    } else if (!creating && detail.data) {
      setDraft({ ...detail.data.definition });
    } else if (creating && !copyFrom) {
      setDraft(emptyDefinition('job'));
    } else return;
    initializedKey.current = editorKey;
    setDirty(false);
    setLoadedKey(editorKey);
  }, [creating, copyFrom, source.data, detail.data, editorKey]);

  useEffect(() => {
    if (!dirty) return;
    const onBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = '';
    };
    const onLinkClick = (event: MouseEvent) => {
      const anchor = (event.target as Element).closest('a[href]');
      if (!anchor || anchor.getAttribute('target') || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      const destination = new URL(anchor.getAttribute('href')!, window.location.href);
      if (destination.origin !== window.location.origin || destination.href === window.location.href) return;
      if (!window.confirm('Discard unsaved definition changes?')) {
        event.preventDefault();
        event.stopPropagation();
      }
    };
    window.addEventListener('beforeunload', onBeforeUnload);
    document.addEventListener('click', onLinkClick, true);
    return () => {
      window.removeEventListener('beforeunload', onBeforeUnload);
      document.removeEventListener('click', onLinkClick, true);
    };
  }, [dirty]);

  const patch = (changes: Partial<Definition>) => {
    setDraft(previous => ({ ...previous, ...changes }));
    setDirty(true);
  };

  const { text: tomlText, lineOf } = definitionToToml(draft);
  const issues = validateDefinition(draft, lineOf);
  const errors = issues.filter(issue => issue.severity === 'error');
  const hints = issues.filter(issue => issue.severity === 'hint');

  const submit = () => {
    if (errors.length > 0) {
      setTab('toml');
      return;
    }
    setServerError('');
    // Strip server-side metadata; keep operator fields only.
    const payload: Definition = { ...draft };
    delete payload.definition_id;
    if (!payload.command) delete payload.command;
    if (!payload.argv || payload.argv.length === 0) delete payload.argv;
    save.mutate(
      creating ? { definition: payload } : { name: existingName, definition: payload, revision: draft.revision },
      {
        onSuccess: saved => {
          setDirty(false);
          if (creating) void navigate(jobPath(saved.name));
          else {
            setDraft(saved);
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
  if (!creating && detail.isError && !detail.data) {
    return (
      <div role="alert" className="rounded-lg border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-300">
        Failed to load {existingName}: {errorText(detail.error)}
      </div>
    );
  }
  if (creating && copyFrom && source.isError && !source.data) {
    return <div role="alert" className="rounded-lg border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-300">Failed to load {copyFrom} for duplication: {errorText(source.error)} <button type="button" className="ml-2 underline" onClick={() => void source.refetch()}>Retry</button></div>;
  }
  if (loadedKey !== editorKey) return <div className="skeleton h-64 w-full" />;

  const isWorker = draft.kind === 'worker';
  const runAsEnabled = daemon.data?.capabilities.includes('run-as') ?? false;

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
          {savedFlash && <span className="chip chip-success">Saved</span>}
        </div>
        <div className="flex items-center gap-2">
          <Link href={creating || !existingName ? '/jobs' : jobPath(existingName)} className="btn-sub">
            Cancel
          </Link>
          <button type="button" className="btn-primary-x" onClick={submit} disabled={save.isPending}>
            {save.isPending && <Icon name="loader" size={14} className="spin" />}
            {creating ? 'Review changes' : 'Save changes'}
          </button>
        </div>
      </div>

      {serverError && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-3 text-sm text-red-300">
          {serverError}
        </div>
      )}
      {(detail.isError || source.isError) && (
        <div role="alert" className="panel border-amber-500/40 bg-amber-500/5 px-4 py-3 text-sm text-amber-300">
          The latest definition could not be refreshed. Your draft is preserved; saving may require reloading if the server revision changed.
        </div>
      )}
      {errors.length > 0 && (
        <div role="alert" className="panel border-red-500/40 bg-red-500/5 px-4 py-2.5 text-sm text-red-300">
          Fix {errors.length} validation {errors.length === 1 ? 'issue' : 'issues'} before saving — see the panel on the right.
        </div>
      )}

      {/* Two-column editor: form 8/12, live TOML + validation 4/12 on desktop. */}
      <div className="grid gap-4 xl:grid-cols-12">
        <div className="min-w-0 xl:col-span-8">
          <JobForm
            showKindTabs={creating && !copyFrom}
            nameLocked={!creating}
            draft={draft}
            readOnly={save.isPending}
            runAsEnabled={runAsEnabled}
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
        <EditExtras name={existingName} onDelete={() => remove.mutate(existingName, { onSuccess: () => navigate('/jobs') })} deleting={remove.isPending} />
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
function EditExtras({ name, onDelete, deleting }: { name: string; onDelete: () => void; deleting: boolean }) {
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
            Removes the definition. Past runs and logs are kept until retention cleans them.
          </p>
          <button type="button" className="btn-danger-x" disabled={deleting} onClick={onDelete}>
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
    ['Timeout', draft.timeout === undefined ? '—' : `${draft.timeout} seconds`],
    ['Grace', draft.grace === undefined ? '—' : `${draft.grace} seconds`],
    ['Overlap', draft.on_overlap ?? '—'],
    ['Catch up', draft.catch_up ?? '—'],
    ['Run on start', draft.run_on_start ? 'yes' : 'no'],
    ['Env base', draft.env_base ?? 'clean'],
    ['Env file', draft.env_file ?? '—'],
    ['Keep runs', String(draft.keep_runs ?? 0)],
    ['Keep for', draft.keep_for === undefined ? '—' : `${draft.keep_for} days`],
    ['Labels', Object.entries(draft.labels ?? {}).map(([k, v]) => `${k}=${v}`).join(', ') || '—'],
    ['Alerts', (draft.alerts ?? []).join(', ') || '—'],
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
