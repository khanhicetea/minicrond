import { useEffect, useRef, useState } from 'react';
import { Link, useLocation, useSearch } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import JobForm, { formSections } from '../components/JobForm';
import { Badge, Button, Card, CardHeader, Dot, IconButton, InlineAlert, PageHeader, buttonClass } from '../components/ui';
import RunsTable from '../components/RunsTable';
import { errorText } from '../api';
import { daemonQuery, jobQuery, runsQuery, useDeleteJob, useSaveJob } from '../queries';
import { definitionToToml, highlightToml } from '../lib/toml';
import { duplicateDefinition, emptyDefinition, updateDefinitionDraft } from '../lib/definition';
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
  const sourceName = new URLSearchParams(useSearch()).get('from') ?? undefined;
  const [, navigate] = useLocation();
  const creating = !existingName;
  const copyFrom = creating ? sourceName : undefined;
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
  const [validationVisible, setValidationVisible] = useState(false);
  const [copiedToml, setCopiedToml] = useState(false);
  const [serverError, setServerError] = useState('');
  const [savedFlash, setSavedFlash] = useState(false);

  // Query data can change after focus or invalidation. Never replace an open draft.
  useEffect(() => {
    if (initializedKey.current === editorKey) return;
    if (creating && copyFrom && source.data) {
      const { definition } = source.data;
      setDraft(duplicateDefinition(definition));
    } else if (!creating && detail.data) {
      setDraft({ ...detail.data.definition });
    } else if (creating && !copyFrom) {
      setDraft(emptyDefinition('job'));
    } else return;
    initializedKey.current = editorKey;
    setDirty(false);
    setValidationVisible(false);
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
    setDraft(previous => updateDefinitionDraft(previous, changes));
    setDirty(true);
    if (Object.keys(changes).some(field => field !== 'kind')) setValidationVisible(true);
  };

  const { text: tomlText, lineOf } = definitionToToml(draft);
  const issues = validateDefinition(draft, lineOf);
  const errors = issues.filter(issue => issue.severity === 'error');
  const hints = issues.filter(issue => issue.severity === 'hint');

  const copyToml = async () => {
    try {
      await navigator.clipboard.writeText(tomlText);
      setCopiedToml(true);
      setTimeout(() => setCopiedToml(false), 2000);
    } catch {
      setServerError('Clipboard unavailable in this browser.');
    }
  };

  const submit = () => {
    if (draft.source === 'config' || save.isPending) return;
    setValidationVisible(true);
    if (errors.length > 0) {
      const field = errors[0].field;
      document.getElementById(`${FORM_ID}-${field}`)?.scrollIntoView({ block: 'center', behavior: 'smooth' });
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
      <div role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-4 py-3 text-sm text-danger-fg">
        Failed to load {existingName}: {errorText(detail.error)}
      </div>
    );
  }
  if (creating && copyFrom && source.isError && !source.data) {
    return <div role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-4 py-3 text-sm text-danger-fg">Failed to load {copyFrom} for cloning: {errorText(source.error)} <button type="button" className="ml-2 underline" onClick={() => void source.refetch()}>Retry</button></div>;
  }
  if (loadedKey !== editorKey) return <div className="skeleton h-64 w-full" />;

  const isWorker = draft.kind === 'worker';
  const configOwned = !creating && draft.source === 'config';
  const runAsEnabled = daemon.data?.capabilities.includes('run-as') ?? false;
  const showIssues = !creating || validationVisible;
  const fieldErrors: Record<string, string> = {};
  if (showIssues) for (const issue of errors) fieldErrors[issue.field] ??= issue.message;
  const kindLabel = isWorker ? 'worker' : 'job';

  const focusField = (field: string) => {
    const element = document.getElementById(`${FORM_ID}-${field}`);
    element?.scrollIntoView({ block: 'center', behavior: 'smooth' });
    window.setTimeout(() => element?.focus({ preventScroll: true }), 250);
  };
  const jumpTo = (section: string) => document.getElementById(`${FORM_ID}-section-${section}`)?.scrollIntoView({ block: 'start', behavior: 'smooth' });

  const saveButton = !configOwned && (
    <Button variant="primary" busy={save.isPending} onClick={submit}>
      {creating ? `Create ${kindLabel}` : 'Save changes'}
    </Button>
  );

  return (
    <div className="space-y-5">
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link href="/jobs">Jobs &amp; workers</Link>
        <span aria-hidden="true">/</span>
        <span>{creating ? (copyFrom ? `Clone of ${copyFrom}` : `New ${kindLabel}`) : draft.name}</span>
      </nav>

      <PageHeader
        title={creating ? (copyFrom ? `Clone ${copyFrom}` : `New ${kindLabel}`) : <span className="mono">{draft.name}</span>}
        badge={
          <>
            {!creating && <Badge tone={isWorker ? 'ok' : 'info'}>{isWorker ? 'Worker' : 'Job'}</Badge>}
            {savedFlash && <Badge tone="ok" icon="check">Saved</Badge>}
            {configOwned && <Badge tone="info">Config owned · view only</Badge>}
          </>
        }
        subtitle={
          creating
            ? copyFrom
              ? `Settings copied from ${copyFrom}. Give it a new name, review, then create.`
              : 'Fill in the basics, command and schedule; everything else has sensible defaults.'
            : configOwned
              ? 'Defined in the daemon configuration file; edit it there and reload.'
              : undefined
        }
        actions={
          <>
            {!creating && existingName && (
              <Link href={`/jobs/new?from=${encodeURIComponent(existingName)}`} className={buttonClass('secondary')}>
                Clone
              </Link>
            )}
            <Link href={creating || !existingName ? '/jobs' : jobPath(existingName)} className={buttonClass('secondary')}>
              {configOwned ? 'Back' : 'Cancel'}
            </Link>
            {saveButton}
          </>
        }
      />

      {serverError && <InlineAlert>{serverError}</InlineAlert>}
      {(detail.isError || source.isError) && (
        <InlineAlert tone="warn">The latest definition could not be refreshed. Your draft is preserved; saving may require reloading if the server revision changed.</InlineAlert>
      )}

      <div className="editor-grid">
        <JobForm
          showKindTabs={creating && !copyFrom}
          nameLocked={!creating}
          draft={draft}
          readOnly={save.isPending || configOwned}
          runAsEnabled={runAsEnabled}
          onChange={patch}
          formId={FORM_ID}
          errors={fieldErrors}
        />

        <aside className="editor-rail" aria-label="Editor tools">
          <Card as="section" aria-label="Sections">
            <ul className="section-nav">
              {formSections(draft.kind).map((section, index) => {
                const invalid = errors.some(issue => showIssues && sectionOf(issue.field, draft.kind) === section.id);
                return (
                  <li key={section.id}>
                    <button type="button" onClick={() => jumpTo(section.id)}>
                      <span className={`section-step !h-5 !w-5 !text-[0.65rem] ${invalid ? 'invalid' : ''}`}>{index + 1}</span>
                      <span className="flex-1">{section.title}</span>
                      {invalid && <Icon name="alert-circle" size={13} className="text-danger-fg" />}
                    </button>
                  </li>
                );
              })}
            </ul>
          </Card>

          {showIssues && <ValidationCard errors={errors} hints={hints} onSelect={focusField} />}

          <Card as="section" aria-labelledby="toml-title" className="overflow-hidden">
            <CardHeader
              id="toml-title"
              title="TOML preview"
              meta="live"
              actions={<IconButton icon={copiedToml ? 'check' : 'copy'} size="sm" label="Copy TOML to clipboard" onClick={() => void copyToml()} />}
            />
            <div className="toml-view max-h-[26rem] overflow-auto py-2">
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
          </Card>
        </aside>
      </div>

      {!configOwned && (dirty || creating) && (
        <div className="save-bar" role="region" aria-label="Save changes">
          <span className="flex items-center gap-2 text-sm">
            <Dot tone={showIssues && errors.length ? 'danger' : 'info'} />
            {showIssues && errors.length
              ? `${errors.length} ${errors.length === 1 ? 'issue' : 'issues'} to fix before saving`
              : creating
                ? `New ${kindLabel} — not saved yet`
                : 'Unsaved changes'}
          </span>
          {showIssues && errors.length > 0 && (
            <Button size="sm" variant="ghost" onClick={() => focusField(errors[0].field)}>
              Go to first issue
            </Button>
          )}
          <span className="ml-auto flex gap-2">
            <Link href={creating || !existingName ? '/jobs' : jobPath(existingName)} className={buttonClass('ghost')}>
              Discard
            </Link>
            {saveButton}
          </span>
        </div>
      )}

      {!creating && existingName && !configOwned && (
        <EditExtras name={existingName} onDelete={() => remove.mutate(existingName, { onSuccess: () => navigate('/jobs') })} deleting={remove.isPending} />
      )}
    </div>
  );
}

const SECTION_OF: Record<string, string> = {
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

function sectionOf(field: string, kind: string): string {
  const section = SECTION_OF[field] ?? '';
  return kind === 'worker' && section === 'schedule' ? 'lifecycle' : section;
}

function ValidationCard({ errors, hints, onSelect }: { errors: DefinitionIssue[]; hints: DefinitionIssue[]; onSelect: (field: string) => void }) {
  const ok = errors.length === 0;
  const issues = [...errors, ...hints];
  const tone = ok ? (hints.length ? 'warn' : 'ok') : 'danger';
  return (
    <section role={ok ? 'status' : 'alert'} className={`callout callout-${tone === 'ok' ? 'info' : tone}`} aria-label="Configuration check">
      <div className="callout-head">
        <Icon name={ok ? 'circle-check' : 'alert-circle'} size={16} className={`shrink-0 ${ok ? 'text-ok-fg' : 'callout-icon'}`} />
        <h2 className={`callout-title ${ok && !hints.length ? '!text-ok-fg' : ''}`}>Configuration check</h2>
        <span className="text-xs muted">
          {ok ? (hints.length ? `${hints.length} suggestion${hints.length === 1 ? '' : 's'}` : 'No issues found') : `${errors.length} error${errors.length === 1 ? '' : 's'}${hints.length ? ` · ${hints.length} hint${hints.length === 1 ? '' : 's'}` : ''}`}
        </span>
      </div>
      {issues.map(issue => {
        const error = issue.severity === 'error';
        return (
          <button key={`${issue.field}-${issue.message}`} type="button" className="issue-button" onClick={() => onSelect(issue.field)}>
            <Icon name={error ? 'alert-circle' : 'alert-triangle'} size={13} className={`mt-0.5 ${error ? 'text-danger-fg' : 'text-warn-fg'}`} />
            <span className="min-w-0">
              <span className={`flex items-baseline justify-between gap-2 text-xs font-semibold ${error ? 'text-danger-fg' : 'text-warn-fg'}`}>
                <span className="truncate">{issue.title}</span>
                {issue.line && <span className="num shrink-0 opacity-80">TOML line {issue.line}</span>}
              </span>
              <span className="block text-xs muted">{issue.message}</span>
            </span>
          </button>
        );
      })}
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
        <h2 className="mb-2 text-sm font-semibold text-danger-fg">Danger zone</h2>
        <div className="panel flex flex-wrap items-center justify-between gap-3 border-danger/30 px-4 py-3">
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
