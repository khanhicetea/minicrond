import { useState } from 'react';
import { Link, useLocation, useParams } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import LogViewer from '../components/LogViewer';
import { RunStatusDot } from '../components/RunsTable';
import { Badge, Button, Card, CardHeader, Facts, InlineAlert, KeyValues, Segmented, buttonClass, isFailedStatus, runStatusMeta } from '../components/ui';
import { api, errorText } from '../api';
import { useAuthState } from '../auth';
import { jobQuery, runAlertsQuery, runQuery, runsQuery, useStopRun, useTriggerJob } from '../queries';
import { downloadFile } from '../lib/download';
import { formatBytes, formatDayTime, formatSpan, formatTimestamp, shortId, shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';
import { publicURL } from '../lib/base';
import { memoryMiB } from '../lib/resources';
import { isActiveRun, type Run } from '../types';

type FilterKey = 'all' | 'failed' | 'scheduled' | 'manual';

function runSpan(run: Run): string {
  return isActiveRun(run) ? formatSpan(run.started_at ?? run.queued_at) : formatSpan(run.started_at, run.ended_at);
}

/**
 * Run detail, failure first: identity and actions, the headline numbers, the
 * last stderr lines (failed runs), then the log beside timing, process,
 * sibling runs and alert deliveries.
 */
export default function RunDetail() {
  const { mode } = useAuthState();
  const params = useParams();
  const id = params.id ?? '';
  const [, navigate] = useLocation();
  const run = useQuery(runQuery(id));
  const alerts = useQuery({ ...runAlertsQuery(id), enabled: Boolean(id) && run.isSuccess });
  const stop = useStopRun();
  const trigger = useTriggerJob();
  const [actionError, setActionError] = useState('');
  const [copied, setCopied] = useState<'' | 'id' | 'curl'>('');
  const [filter, setFilter] = useState<FilterKey>('all');

  const jobId = run.data?.job ?? '';
  const jobDetail = useQuery({ ...jobQuery(jobId), enabled: Boolean(jobId) });
  const siblings = useQuery({ ...runsQuery(jobId, 30), enabled: Boolean(jobId) });

  if (run.isPending) return <div className="skeleton h-64 w-full" />;
  if (run.isError) return <InlineAlert>Failed to load run: {errorText(run.error)}</InlineAlert>;

  const data = run.data;
  const meta = runStatusMeta(data.status);
  const active = isActiveRun(data);
  const failed = isFailedStatus(data.status);
  const definition = jobDetail.data?.definition;
  const configOwned = definition?.source === 'config';
  const usage = data.resource_usage;
  const triggerLabel = data.trigger === 'manual' ? 'Manual run' : data.trigger === 'schedule' ? 'Scheduled run' : `${data.trigger} run`;

  const filteredSiblings = (siblings.data ?? []).filter(sibling => {
    if (filter === 'failed') return isFailedStatus(sibling.status);
    if (filter === 'scheduled') return sibling.trigger === 'schedule';
    if (filter === 'manual') return sibling.trigger === 'manual';
    return true;
  });

  const copy = async (what: 'id' | 'curl') => {
    try {
      await navigator.clipboard.writeText(what === 'id' ? data.run_id : `curl -s -H "Authorization: Bearer $MINICROND_TOKEN" ${publicURL(`/api/v1/runs/${data.run_id}`)}`);
      setCopied(what);
      setTimeout(() => setCopied(''), 2000);
    } catch {
      setActionError('Clipboard unavailable in this browser.');
    }
  };

  const rerun = () => {
    setActionError('');
    trigger.mutate(data.job, {
      onSuccess: next => navigate(`/runs/${encodeURIComponent(next.run_id)}`),
      onError: err => setActionError(errorText(err)),
    });
  };

  const summary = [
    data.scheduled_for ? `Scheduled for ${formatDayTime(data.scheduled_for)}` : triggerLabel,
    data.attempt > 1 ? `attempt ${data.attempt}` : '',
    active ? `running for ${runSpan(data)}` : failed && data.exit_code === 0 ? `${meta.label.toLowerCase()} after ${runSpan(data)}` : data.exit_code !== undefined ? `exited with code ${data.exit_code}${data.signal ? ` (${data.signal})` : ''} after ${runSpan(data)}` : data.end_reason || '',
  ]
    .filter(Boolean)
    .join(' · ');

  return (
    <div className="space-y-5">
      <nav className="crumbs" aria-label="Breadcrumb">
        <Link href="/runs">Runs</Link>
        <span aria-hidden="true">/</span>
        <Link href={`/runs?job=${encodeURIComponent(data.job)}`}>{data.job}</Link>
        <span aria-hidden="true">/</span>
        <button type="button" className="inline-flex items-center gap-1 mono text-fg-muted hover:text-fg" title="Copy full run ID" onClick={() => void copy('id')}>
          #{shortRunId(data.run_id)}
          <Icon name={copied === 'id' ? 'check' : 'copy'} size={11} />
        </button>
      </nav>

      <header className="page-header !items-start">
        <div className="min-w-0 flex-[1_1_22rem]">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="page-title mono">{data.job}</h1>
            <Badge tone={meta.tone} icon={meta.icon} spin={meta.spin} className="text-[0.8125rem]">
              {meta.label}
            </Badge>
          </div>
          <p className="page-subtitle">{summary}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Link href={jobPath(data.job)} className={buttonClass('secondary')}>
            {configOwned ? 'View definition' : 'Edit definition'}
          </Link>
          {mode === 'token' && (
            <Button icon={copied === 'curl' ? 'check' : 'terminal'} onClick={() => void copy('curl')}>
              {copied === 'curl' ? 'Copied' : 'Copy as curl'}
            </Button>
          )}
          <Button
            icon="download"
            onClick={() => {
              setActionError('');
              downloadFile(api.rawLogUrl(id), 'minicron-run.log', api.download).catch(err => setActionError(errorText(err)));
            }}
          >
            Download log
          </Button>
          {active ? (
            <Button variant="danger" icon="square" busy={stop.isPending} onClick={() => stop.mutate(id, { onError: err => setActionError(errorText(err)) })}>
              Stop
            </Button>
          ) : (
            !configOwned && (
              <Button variant="primary" icon="play" busy={trigger.isPending} onClick={rerun}>
                Run again
              </Button>
            )
          )}
        </div>
      </header>

      <Facts
        label="Run summary"
        items={[
          {
            label: 'Exit code',
            value: data.exit_code === undefined ? '—' : `${data.exit_code}${data.signal ? ` (${data.signal})` : ''}`,
            tone: data.exit_code === undefined ? undefined : data.exit_code === 0 ? 'ok' : 'danger',
          },
          { label: active ? 'Running for' : 'Duration', value: runSpan(data), tone: active ? 'info' : undefined },
          ...(usage ? [{ label: 'Peak RSS', value: memoryMiB(usage.peak_rss_bytes), title: 'Peak RSS of the direct process; not the combined process-tree peak.' }] : []),
          { label: 'Started', value: data.started_at ? formatDayTime(data.started_at) : '—' },
          { label: 'Log size', value: `${formatBytes(data.log_bytes)}${data.log_truncated ? ' · truncated' : ''}` },
        ]}
      />

      {actionError && <InlineAlert>{actionError}</InlineAlert>}
      {data.log_truncated && <InlineAlert tone="warn">Stored log exceeded the size cap and was truncated.</InlineAlert>}

      <div className="flex flex-wrap items-start gap-5">
        <Card as="section" aria-label="Run output" className="flex min-h-[34rem] min-w-0 flex-[999_1_34rem] flex-col overflow-hidden">
          <LogViewer runId={id} errorTail={failed} />
        </Card>

        <aside className="flex min-w-0 flex-[1_1_19rem] flex-col gap-4" aria-label="Run information">
          <Card as="section" aria-labelledby="timing">
            <CardHeader id="timing" title="Timing" />
            <div className="px-4 py-3">
              <KeyValues
                items={[
                  { label: 'Queued', value: formatTimestamp(data.queued_at) },
                  { label: 'Scheduled for', value: data.scheduled_for ? formatTimestamp(data.scheduled_for) : '—' },
                  { label: 'Started', value: formatTimestamp(data.started_at) },
                  { label: 'Ended', value: formatTimestamp(data.ended_at) },
                  { label: 'End reason', value: data.end_reason || '—' },
                ]}
              />
            </div>
          </Card>

          <Card as="section" aria-labelledby="process">
            <CardHeader id="process" title="Process" />
            <div className="px-4 py-3">
              <KeyValues
                items={[
                  { label: 'PID / PGID', value: data.pid || data.pgid ? `${data.pid || '—'} / ${data.pgid || '—'}` : '—', mono: true },
                  { label: 'Run as', value: definition?.run_as || '—', mono: Boolean(definition?.run_as) },
                  ...(usage
                    ? [
                        { label: 'CPU user / sys', value: `${(usage.user_cpu_us / 1e6).toFixed(3)}s / ${(usage.system_cpu_us / 1e6).toFixed(3)}s` },
                        { label: 'Peak RSS', value: memoryMiB(usage.peak_rss_bytes) },
                      ]
                    : []),
                  { label: 'Revision', value: `r${data.revision}` },
                  { label: 'Definition hash', value: shortId(data.definition_hash, 16), mono: true },
                ]}
              />
              {usage && <p className="mt-3 text-xs faint">Kernel exit accounting; CPU may include waited-for descendants.</p>}
            </div>
          </Card>

          <Card as="section" aria-labelledby="siblings">
            <CardHeader id="siblings" title="Recent runs" meta={filteredSiblings.length} />
            <div className="px-3 pt-3">
              <Segmented<FilterKey>
                size="sm"
                label="Filter recent runs"
                value={filter}
                onChange={setFilter}
                options={[
                  { value: 'all', label: 'All' },
                  { value: 'failed', label: 'Failed' },
                  { value: 'scheduled', label: 'Scheduled' },
                  { value: 'manual', label: 'Manual' },
                ]}
              />
            </div>
            <ol className="m-0 max-h-[22rem] list-none overflow-y-auto p-2">
              {filteredSiblings.length === 0 && <li className="px-2 py-4 text-center text-sm faint">No runs match this filter.</li>}
              {filteredSiblings.map(sibling => (
                <li key={sibling.run_id}>
                  <Link href={`/runs/${encodeURIComponent(sibling.run_id)}`} className="runs-history-row" aria-current={sibling.run_id === id ? 'page' : undefined}>
                    <RunStatusDot status={sibling.status} />
                    <span className="flex min-w-0 flex-1 items-center justify-between gap-2 text-[0.78rem]">
                      <span className="truncate mono">{formatDayTime(sibling.started_at ?? sibling.queued_at)}</span>
                      <span className="shrink-0 faint tabular">{runSpan(sibling)}</span>
                    </span>
                  </Link>
                </li>
              ))}
            </ol>
            <div className="border-t border-line-subtle px-4 py-2.5">
              <Link href={`/runs?job=${encodeURIComponent(data.job)}`} className="text-sm text-link hover:text-link-hover">
                Open in Runs
              </Link>
            </div>
          </Card>

          <Card as="section" aria-labelledby="alerts">
            <CardHeader id="alerts" title="Alert deliveries" />
            <div className="px-4 py-3 text-sm">
              {alerts.isPending ? (
                <p className="muted">Loading alert deliveries…</p>
              ) : alerts.isError ? (
                <p role="alert" className="text-danger-fg">
                  Failed to load alert deliveries: {errorText(alerts.error)}
                </p>
              ) : alerts.data.length === 0 ? (
                <p className="muted">No alert deliveries for this run.</p>
              ) : (
                <ul className="m-0 list-none space-y-2.5 p-0">
                  {alerts.data.map(item => (
                    <li key={item.channel}>
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="min-w-0 flex-1 break-all mono">{item.channel}</span>
                        <Badge tone={item.status === 'sent' ? 'ok' : ['failed', 'dropped', 'interrupted'].includes(item.status) ? 'danger' : 'info'}>{item.status}</Badge>
                      </div>
                      <div className="mt-0.5 text-xs faint">
                        {item.attempts} {item.attempts === 1 ? 'attempt' : 'attempts'} · updated {formatTimestamp(item.updated_at)}
                      </div>
                      {item.last_error && <p className="mt-1 break-words text-xs text-danger-fg">Last error: {item.last_error}</p>}
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </Card>
        </aside>
      </div>
    </div>
  );
}
