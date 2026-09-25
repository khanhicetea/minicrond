import { Fragment, useEffect, useRef, useState } from 'react';
import { ApiError, api, errorText, streamRunLogs } from '../api';
import { Icon } from './Icon';
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

/**
 * Live log viewer backed by the daemon's resumable SSE stream, styled after
 * the reference run-output panel: stream filters, text search, wrap toggle,
 * line cap, and follow/pause controls.
 */
export default function LogViewer({ runId, footer }: { runId: string; footer?: React.ReactNode }) {
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
      ? 'loading…'
      : phase === 'backlog'
        ? 'reconnecting…'
        : phase === 'live'
          ? 'live'
          : phase === 'done'
            ? 'stream closed'
            : phase === 'dropped'
              ? 'frames dropped by retention'
              : 'error';

  const phaseCls =
    phase === 'live'
      ? 'chip-success'
      : phase === 'error' || phase === 'dropped'
        ? 'chip-error'
        : phase === 'done'
          ? 'chip-neutral'
          : 'chip-info';

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-2 border-b border-base-300 px-3 py-2">
        <div className="flex items-center gap-1">
          {(['all', 'stdout', 'stderr', 'system'] as StreamFilter[]).map(filter => (
            <button
              key={filter}
              type="button"
              onClick={() => setStreamFilter(filter)}
              className={`rounded-md border px-2 py-1 text-xs font-medium transition-colors ${
                streamFilter === filter
                  ? filter === 'stderr'
                    ? 'border-red-500/50 bg-red-500/15 text-red-300'
                    : filter === 'system'
                      ? 'border-blue-500/50 bg-blue-500/15 text-blue-300'
                      : filter === 'stdout'
                        ? 'border-green-500/50 bg-green-500/15 text-green-300'
                        : 'border-blue-500/50 bg-blue-500/15 text-blue-300'
                  : 'border-transparent text-[color-mix(in_srgb,var(--color-base-content)_55%,transparent)] hover:bg-base-300/40'
              }`}
            >
              {filter}
            </button>
          ))}
        </div>
        <div className="ml-1 flex min-w-[10rem] flex-1 items-center gap-1.5 rounded-lg border border-base-300 bg-base-200 px-2 py-1">
          <Icon name="search" size={13} className="faint shrink-0" />
          <input
            value={query}
            onChange={event => setQuery(event.target.value)}
            placeholder="Search logs"
            className="w-full bg-transparent font-mono text-xs outline-none placeholder:text-[color-mix(in_srgb,var(--color-base-content)_35%,transparent)]"
            aria-label="Search logs"
          />
        </div>
        <label className="flex items-center gap-1.5 text-xs muted" title="Wrap long lines">
          <Icon name="wrap-text" size={14} />
          Wrap
          <span className="switch !h-[1.1rem] !w-[2rem]">
            <input type="checkbox" checked={wrap} onChange={event => setWrap(event.target.checked)} />
            <span className="track" />
          </span>
        </label>
        <select
          value={limit}
          onChange={event => setLimit(Number(event.target.value))}
          aria-label="Line limit"
          className="mc-select !w-auto !py-1 !text-xs"
        >
          {[500, 1000, 5000].map(value => (
            <option key={value} value={value}>
              {value} lines
            </option>
          ))}
        </select>
        <button
          type="button"
          onClick={() => (follow ? setFollow(false) : resumeFollow())}
          className={`btn-sub !py-1 !px-2 !text-xs ${follow ? '!border-green-500/50 !text-green-300' : ''}`}
          title={follow ? 'Stop automatic scrolling' : 'Follow newest lines'}
        >
          <span className={`dot ${follow ? 'dot-green dot-pulse' : 'dot-gray'}`} />
          {follow ? 'Following' : 'Follow newest'}
        </button>
      </div>

      {/* Status strip */}
      <div className="flex items-center gap-2 border-b border-base-300 px-3 py-1.5 text-xs">
        <span className={`chip ${phaseCls} !py-0.5`}>{phaseLabel}</span>
        <span className="muted">
          {`${visible.length} shown · ${filtered.length} matching · ${lines.length} buffered`}
        </span>
        {pageCount > 1 && (
          <div className="ml-auto flex items-center gap-2">
            <button type="button" className="btn-sub !px-2 !py-0.5 !text-xs" disabled={safePage >= pageCount - 1} onClick={() => { setPage(safePage + 1); setFollow(false); stickRef.current = false; }}>
              Older
            </button>
            <span className="muted">Page {pageCount - safePage} of {pageCount}</span>
            <button type="button" className="btn-sub !px-2 !py-0.5 !text-xs" disabled={safePage === 0} onClick={() => setPage(safePage - 1)}>
              Newer
            </button>
          </div>
        )}
        {error && <span className="text-red-400">{error}</span>}
      </div>

      {/* Log body */}
      <div
        ref={containerRef}
        onScroll={onScroll}
        role="log"
        aria-live="off"
        className={`log-view min-h-[16rem] flex-1 overflow-auto py-2 ${wrap ? 'log-wrap' : ''}`}
      >
        {lines.length === 0 && (phase === 'loading' || phase === 'backlog') && (
          <div className="px-3 py-6 text-center faint">waiting for log frames…</div>
        )}
        {lines.length > 0 && visible.length === 0 && (
          <div className="px-3 py-6 text-center faint">no lines match the current filters</div>
        )}
        {visible.map(line => {
          const tag = STREAM_TAG[line.stream] ?? { label: String(line.stream), cls: '' };
          return (
            <div
              key={line.seq}
              className={`log-row ${line.stream === STREAM_STDERR ? 'log-stderr' : line.stream === STREAM_SYSTEM ? 'log-system' : ''}`}
            >
              <span className="log-num">{String(line.seq).padStart(4, '0')}</span>
              <span className="log-ts">[{formatClock(line.ts)}]</span>
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

      {/* Follow footer */}
      {!follow && (
        <div className="flex items-center justify-center gap-3 border-t border-base-300 py-2">
          <button type="button" className="btn-sub !border-green-500/50 !text-green-300" onClick={resumeFollow}>
            <Icon name="rotate-ccw" size={13} />
            Resume follow
          </button>
          <span className="text-xs muted">Automatic scrolling is off; new lines continue to arrive.</span>
        </div>
      )}
      {footer}
    </div>
  );
}
