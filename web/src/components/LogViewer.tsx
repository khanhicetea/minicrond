import { Fragment, useEffect, useRef, useState } from 'react';
import { ApiError, errorText, streamRunLogs } from '../api';
import { decodePayload, parseAnsi } from '../lib/ansi';
import { STREAM_STDERR, STREAM_SYSTEM, type Frame } from '../types';

interface LogLine {
  key: number;
  className: string;
  segments: ReturnType<typeof parseAnsi>;
}

const MAX_LINES = 5_000;
const MAX_RETRIES = 5;

function makeLine(frame: Frame): LogLine {
  return {
    key: frame.sequence,
    className: frame.stream === STREAM_STDERR ? 'log-stderr' : frame.stream === STREAM_SYSTEM ? 'log-system' : '',
    segments: parseAnsi(decodePayload(frame.payload)),
  };
}

function appendLine(lines: LogLine[], line: LogLine): LogLine[] {
  if (lines.length >= MAX_LINES) {
    const trimmed = lines.slice(lines.length - MAX_LINES + 1);
    return [...trimmed, line];
  }
  return [...lines, line];
}

type Phase = 'connecting' | 'backlog' | 'live' | 'done' | 'dropped' | 'error';

const PHASE_BADGE: Record<Phase, string> = {
  connecting: 'badge-ghost',
  backlog: 'badge-info badge-outline',
  live: 'badge-success',
  done: 'badge-ghost',
  dropped: 'badge-warning',
  error: 'badge-error',
};

/** Live log viewer backed by the daemon's resumable SSE stream. */
export default function LogViewer({ runId }: { runId: string }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [phase, setPhase] = useState<Phase>('connecting');
  const [error, setError] = useState('');
  const containerRef = useRef<HTMLDivElement>(null);
  const stickToBottom = useRef(true);

  useEffect(() => {
    const controller = new AbortController();
    setLines([]);
    setPhase('connecting');
    setError('');
    stickToBottom.current = true;

    let sequence = 0;
    let closed = false;

    (async () => {
      for (let attempt = 0; !closed && !controller.signal.aborted; attempt += 1) {
        try {
          await streamRunLogs(runId, sequence, event => {
            switch (event.type) {
              case 'line':
                sequence = Math.max(sequence, event.frame.sequence);
                setLines(prev => appendLine(prev, makeLine(event.frame)));
                setPhase(prev => (prev === 'connecting' ? 'backlog' : prev));
                break;
              case 'backlog_done':
                setPhase('live');
                break;
              case 'done':
                setPhase('done');
                closed = true;
                break;
              case 'dropped':
                setPhase('dropped');
                closed = true;
                break;
            }
          }, controller.signal);
          // Server closed without a terminal event (for example a daemon
          // restart): retry with resume-from-sequence.
          if (!closed && attempt < MAX_RETRIES) {
            setPhase('connecting');
            await new Promise(resolve => setTimeout(resolve, Math.min(1000 * 2 ** attempt, 5000)));
          }
        } catch (err) {
          if (controller.signal.aborted || closed) return;
          if (err instanceof ApiError && err.status === 404) {
            setPhase('error');
            setError('log data not found (it may have been removed by retention)');
            return;
          }
          if (attempt < MAX_RETRIES) {
            setPhase('connecting');
            await new Promise(resolve => setTimeout(resolve, Math.min(1000 * 2 ** attempt, 5000)));
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
    };
  }, [runId]);

  useEffect(() => {
    const element = containerRef.current;
    if (element && stickToBottom.current) element.scrollTop = element.scrollHeight;
  }, [lines]);

  const onScroll = () => {
    const element = containerRef.current;
    if (element) stickToBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight < 48;
  };

  const jumpToLatest = () => {
    const element = containerRef.current;
    if (element) {
      stickToBottom.current = true;
      element.scrollTop = element.scrollHeight;
    }
  };

  return (
    <div>
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <span className={`badge badge-sm ${PHASE_BADGE[phase]}`}>{phase}</span>
        <span className="text-xs text-base-content/60">{lines.length} lines</span>
        {error && <span className="text-xs text-error">{error}</span>}
        <button type="button" className="btn btn-ghost btn-xs ml-auto" onClick={jumpToLatest}>
          Jump to latest ↓
        </button>
      </div>
      <div
        ref={containerRef}
        onScroll={onScroll}
        role="log"
        aria-live="polite"
        className="log-view h-[60vh] overflow-auto rounded-box border border-base-300 bg-neutral-950 p-3 text-neutral-200"
      >
        {lines.length === 0 && phase === 'connecting' && (
          <span className="loading loading-dots loading-sm text-base-content/50" aria-label="connecting" />
        )}
        {lines.map(line => (
          <div key={line.key} className={`log-line ${line.className}`}>
            {line.segments.map((segment, index) =>
              segment.classes.length ? (
                <span key={index} className={segment.classes.join(' ')}>
                  {segment.text}
                </span>
              ) : (
                <Fragment key={index}>{segment.text}</Fragment>
              ),
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
