import { Fragment, useEffect, useRef, useState } from 'react';
import { ApiError, api, errorText, streamRunLogs } from '../api';
import { Icon } from './Icon';
import { Badge, Button, SearchField, Segmented, type Tone } from './ui';
import { decodePayload, parseAnsi } from '../lib/ansi';
import { formatClock } from '../lib/format';
import { STREAM_STDERR, STREAM_SYSTEM, type Frame } from '../types';

interface LogLine {
  seq: number;
  ts: string;
  stream: number;
  segments: ReturnType<typeof parseAnsi>;
  searchText: string;
}

type Phase = 'loading' | 'backlog' | 'live' | 'done' | 'dropped' | 'error';
type StreamFilter = 'all' | 'stdout' | 'stderr' | 'system';

const MAX_BUFFER = 20_000;
const MAX_RETRIES = 5;
const PAGE_SIZE = 500;
const FLUSH_MS = 50;

function makeLine(frame: Frame): LogLine {
  const segments = parseAnsi(decodePayload(frame.payload));
  return { seq: frame.sequence, ts: frame.timestamp, stream: frame.stream, segments, searchText: segments.map(segment => segment.text).join('').toLowerCase() };
}

const STREAM_TAG: Record<number, { label: string; cls: string }> = {
  1: { label: 'stdout', cls: 'stdout' },
  2: { label: 'stderr', cls: 'stderr' },
  3: { label: 'system', cls: 'system' },
};

const TAIL_LINES = 3;
const COPY_LINES = 20;

function lineText(line: LogLine): string {
  return line.segments.map(segment => segment.text).join('');
}

/**
 * Live log viewer backed by the daemon's resumable SSE stream, styled after
 * the reference run-output panel: stream filters, text search, wrap toggle,
 * line cap, and follow/pause controls.
 */
export default function LogViewer({ runId, footer, errorTail = false }: { runId: string; footer?: React.ReactNode; errorTail?: boolean }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [phase, setPhase] = useState<Phase>('loading');
  const [error, setError] = useState('');
  const [streamFilter, setStreamFilter] = useState<StreamFilter>('all');
  const [query, setQuery] = useState('');
  const [wrap, setWrap] = useState(true);
  const [limit, setLimit] = useState(1000);
  const [follow, setFollow] = useState(true);
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState('');

  const containerRef = useRef<HTMLDivElement>(null);
  const stickRef = useRef(true);
  const linesRef = useRef<LogLine[]>([]);

  useEffect(() => {
    const timer = window.setTimeout(() => setSearch(query.trim().toLowerCase()), 150);
    return () => window.clearTimeout(timer);
  }, [query]);

  useEffect(() => {
    const controller = new AbortController();
    linesRef.current = [];
    setLines([]);
    setPhase('loading');
    setError('');
    stickRef.current = true;
    setFollow(true);
    setPage(0);

    let sequence = 0;
    let closed = false;
    let pending: Frame[] = [];
    let flushTimer: number | undefined;
    let retryTimer: number | undefined;
    let retryResolve: (() => void) | undefined;

    const flush = () => {
      if (flushTimer !== undefined) window.clearTimeout(flushTimer);
      flushTimer = undefined;
      if (closed || pending.length === 0) return;
      const frames = pending;
      pending = [];
      const merged = linesRef.current.concat(frames.map(makeLine));
      linesRef.current = merged.length > MAX_BUFFER ? merged.slice(merged.length - MAX_BUFFER) : merged;
      setLines(linesRef.current);
    };

    const push = (frames: Frame[]) => {
      if (frames.length === 0 || closed) return;
      pending.push(...frames);
      if (flushTimer === undefined) flushTimer = window.setTimeout(flush, FLUSH_MS);
    };

    const retry = (attempt: number) => new Promise<void>(resolve => {
      retryResolve = resolve;
      retryTimer = window.setTimeout(() => {
        retryTimer = undefined;
        retryResolve = undefined;
        resolve();
      }, Math.min(1000 * 2 ** attempt, 5000));
    });

    (async () => {
      for (let attempt = 0; !closed && !controller.signal.aborted; attempt += 1) {
        try {
          if (sequence === 0) {
            const backlog = await api.logFrames(runId, 0, 5000, controller.signal);
            if (controller.signal.aborted) return;
            push(backlog.items);
            if (backlog.items.length > 0) sequence = Math.max(...backlog.items.map(item => item.sequence));
          }
          await streamRunLogs(runId, sequence, event => {
            switch (event.type) {
              case 'line':
                if (event.frame.sequence <= sequence) break;
                sequence = event.frame.sequence;
                push([event.frame]);
                break;
              case 'backlog_done':
                setPhase('live');
                break;
              case 'done':
                flush();
                setPhase('done');
                controller.abort();
                break;
              case 'dropped':
                flush();
                setPhase('dropped');
                controller.abort();
                break;
            }
          }, controller.signal);
          if (controller.signal.aborted) return;
          if (attempt < MAX_RETRIES) {
            setPhase('backlog');
            await retry(attempt);
          } else {
            setPhase('error');
            setError('Log stream disconnected. Reload this run to retry.');
            return;
          }
        } catch (err) {
          if (controller.signal.aborted || closed) return;
          if (err instanceof ApiError && [400, 401, 403, 404].includes(err.status)) {
            setPhase('error');
            setError(err.status === 404 ? 'Log data not found (it may have been removed by retention).' : errorText(err));
            return;
          }
          if (attempt < MAX_RETRIES) {
            setPhase('backlog');
            await retry(attempt);
            continue;
          }
          setPhase('error');
          setError(errorText(err));
          return;
        }
      }
    })();

    return () => {
      closed = true;
      controller.abort();
      if (flushTimer !== undefined) window.clearTimeout(flushTimer);
      if (retryTimer !== undefined) window.clearTimeout(retryTimer);
      retryResolve?.();
      pending = [];
    };
  }, [runId]);

  const filtered = lines.filter(line => {
    if (streamFilter === 'stderr' && line.stream !== STREAM_STDERR) return false;
    if (streamFilter === 'system' && line.stream !== STREAM_SYSTEM) return false;
    if (streamFilter === 'stdout' && line.stream !== 1) return false;
    return !search || line.searchText.includes(search);
  });
  const scoped = filtered.slice(Math.max(0, filtered.length - limit));
  const pageCount = Math.max(1, Math.ceil(scoped.length / PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const end = scoped.length - safePage * PAGE_SIZE;
  const visible = scoped.slice(Math.max(0, end - PAGE_SIZE), end);

  useEffect(() => setPage(0), [streamFilter, search, limit]);

  useEffect(() => {
    const element = containerRef.current;
    if (element && follow && stickRef.current && safePage === 0) element.scrollTop = element.scrollHeight;
  }, [lines, streamFilter, search, limit, page, follow, safePage]);

  const onScroll = () => {
    const element = containerRef.current;
    if (!element) return;
    const nearBottom = element.scrollHeight - element.scrollTop - element.clientHeight < 48;
    stickRef.current = nearBottom;
    if (!nearBottom && follow) setFollow(false);
  };

  const resumeFollow = () => {
    setFollow(true);
    setPage(0);
    stickRef.current = true;
    const element = containerRef.current;
    if (element) element.scrollTop = element.scrollHeight;
  };

  const phaseLabel =
    phase === 'loading'
      ? 'Loading…'
      : phase === 'backlog'
        ? 'Reconnecting…'
        : phase === 'live'
          ? 'Live'
          : phase === 'done'
            ? 'Complete'
            : phase === 'dropped'
              ? 'Frames dropped by retention'
              : 'Error';
  const phaseTone: Tone = phase === 'live' ? 'ok' : phase === 'error' || phase === 'dropped' ? 'danger' : phase === 'done' ? 'neutral' : 'info';

  // Failed runs: surface the last stderr lines from the buffer, no extra request.
  const finished = phase === 'done' || phase === 'dropped';
  const stderrLines = errorTail && finished ? lines.filter(line => line.stream === STREAM_STDERR) : [];
  const tail = stderrLines.slice(-TAIL_LINES);
  const [copiedTail, setCopiedTail] = useState(false);
  const [flashSeq, setFlashSeq] = useState<number | null>(null);

  const jumpTo = (seq: number) => {
    setStreamFilter('all');
    setQuery('');
    setSearch('');
    setFollow(false);
    stickRef.current = false;
    // Find the page that holds the line (pages count back from the newest).
    const index = lines.findIndex(line => line.seq === seq);
    const scopedStart = Math.max(0, lines.length - limit);
    if (index < scopedStart) setLimit(5000);
    const fromEnd = lines.length - 1 - index;
    setPage(Math.floor(fromEnd / PAGE_SIZE));
    setFlashSeq(seq);
    window.setTimeout(() => {
      containerRef.current?.querySelector(`[data-seq="${seq}"]`)?.scrollIntoView({ block: 'center' });
    }, 30);
  };

  const copyTail = async () => {
    try {
      await navigator.clipboard.writeText(stderrLines.slice(-COPY_LINES).map(lineText).join('\n').trim());
      setCopiedTail(true);
      window.setTimeout(() => setCopiedTail(false), 2000);
    } catch {
      setCopiedTail(false);
    }
  };

  return (
    <div className="flex h-full min-h-0 flex-1 flex-col">
      {tail.length > 0 && (
        <div className="error-tail" aria-label="Last stderr before exit">
          <span className="text-xs font-semibold text-danger-fg">Last stderr before exit</span>
          <span className="text-xs muted">
            {tail.length} {tail.length === 1 ? 'line' : 'lines'} · from line {tail[0].seq}
          </span>
          <span className="ml-auto flex gap-2">
            <Button size="sm" variant="ghost" icon={copiedTail ? 'check' : 'copy'} onClick={() => void copyTail()}>
              {copiedTail ? 'Copied' : 'Copy last errors'}
            </Button>
            <Button size="sm" variant="danger" onClick={() => jumpTo(tail[0].seq)}>
              Jump to line {tail[0].seq}
            </Button>
          </span>
          <pre>{tail.map(lineText).join('\n')}</pre>
        </div>
      )}

      <div className="log-toolbar">
        <SearchField size="sm" value={query} onChange={setQuery} label="Search logs" className="min-w-[9rem] flex-[1_1_10rem] max-w-[16rem]" />
        <Segmented<StreamFilter>
          size="sm"
          label="Streams"
          value={streamFilter}
          onChange={setStreamFilter}
          options={[
            { value: 'all', label: 'All' },
            { value: 'stdout', label: 'stdout' },
            { value: 'stderr', label: 'stderr' },
            { value: 'system', label: 'system' },
          ]}
        />
        <span className="ml-auto" />
        <button type="button" className={`pill ${wrap ? 'active' : ''}`} aria-pressed={wrap} onClick={() => setWrap(value => !value)} title="Wrap long lines">
          <Icon name="wrap-text" size={13} />
          Wrap
        </button>
        <select value={limit} onChange={event => setLimit(Number(event.target.value))} aria-label="Line limit" className="mc-select !min-h-[1.875rem] !w-auto !py-0 !text-xs">
          {[500, 1000, 5000].map(value => (
            <option key={value} value={value}>
              {value} lines
            </option>
          ))}
        </select>
        <button
          type="button"
          className={`pill ${follow ? 'active' : ''}`}
          aria-pressed={follow}
          onClick={() => (follow ? setFollow(false) : resumeFollow())}
          title={follow ? 'Stop automatic scrolling' : 'Follow newest lines'}
        >
          <span className={`dot ${follow ? 'dot-blue dot-pulse' : 'dot-gray'}`} />
          {follow ? 'Following' : 'Follow'}
        </button>
      </div>

      <div className="flex flex-wrap items-center gap-2 border-b border-line-subtle px-4 py-1.5 text-xs">
        <Badge tone={phaseTone}>{phaseLabel}</Badge>
        <span className="faint">{`${visible.length} shown · ${filtered.length} matching · ${lines.length} buffered`}</span>
        {pageCount > 1 && (
          <div className="ml-auto flex items-center gap-2">
            <Button
              size="sm"
              disabled={safePage >= pageCount - 1}
              onClick={() => {
                setPage(safePage + 1);
                setFollow(false);
                stickRef.current = false;
              }}
            >
              Older
            </Button>
            <span className="muted">
              Page {pageCount - safePage} of {pageCount}
            </span>
            <Button size="sm" disabled={safePage === 0} onClick={() => setPage(safePage - 1)}>
              Newer
            </Button>
          </div>
        )}
        {error && <span className="text-danger-fg">{error}</span>}
      </div>

      <div ref={containerRef} onScroll={onScroll} role="log" aria-live="off" className={`log-view min-h-[16rem] flex-1 overflow-auto py-2 ${wrap ? 'log-wrap' : ''}`}>
        {lines.length === 0 && (phase === 'loading' || phase === 'backlog') && <div className="px-4 py-6 text-center faint">Waiting for log frames…</div>}
        {lines.length === 0 && phase === 'done' && <div className="px-4 py-6 text-center faint">This run produced no output.</div>}
        {lines.length > 0 && visible.length === 0 && <div className="px-4 py-6 text-center faint">No lines match the current filters.</div>}
        {visible.map(line => {
          const tag = STREAM_TAG[line.stream] ?? { label: String(line.stream), cls: '' };
          return (
            <div
              key={line.seq}
              data-seq={line.seq}
              className={`log-row ${line.stream === STREAM_STDERR ? 'log-stderr' : line.stream === STREAM_SYSTEM ? 'log-system' : ''} ${flashSeq === line.seq ? 'flash' : ''}`}
            >
              <span className="log-num">{line.seq}</span>
              <span className="log-ts">{formatClock(line.ts)}</span>
              <span className={`log-kind ${tag.cls}`} role="img" aria-label={tag.label} title={tag.label} />
              <span className="log-text">
                {line.segments.map((segment, index) =>
                  segment.classes.length ? (
                    <span key={index} className={segment.classes.join(' ')}>
                      {segment.text}
                    </span>
                  ) : (
                    <Fragment key={index}>{segment.text}</Fragment>
                  ),
                )}
              </span>
            </div>
          );
        })}
      </div>

      {!follow && phase === 'live' && (
        <div className="flex flex-wrap items-center justify-center gap-3 border-t border-line-subtle py-2">
          <Button size="sm" icon="rotate-ccw" onClick={resumeFollow}>
            Resume follow
          </Button>
          <span className="text-xs muted">Automatic scrolling is off; new lines continue to arrive.</span>
        </div>
      )}
      {footer}
    </div>
  );
}
