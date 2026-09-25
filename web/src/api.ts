import { auth } from './auth';
import { publicPath } from './lib/base';
import type { AlertChannel, DaemonInfo, Definition, Frame, JobDetail, Run, RunAlert, RunMetrics, WorkerState } from './types';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export function errorText(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  /** Override the stored token (used by login validation). */
  token?: string;
  signal?: AbortSignal;
  headers?: Record<string, string>;
  revokeOn401?: boolean;
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const token = options.token ?? auth.token;
  const headers: Record<string, string> = { ...options.headers };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(publicPath(path), {
    method: options.method ?? 'GET',
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    signal: options.signal,
  });
  if (!response.ok) {
    if (response.status === 401 && options.revokeOn401 !== false) auth.revoke();
    let code = 'request_failed';
    let message = response.statusText || 'request failed';
    try {
      const envelope = (await response.json()) as { error?: { code?: string; message?: string } };
      if (envelope.error) {
        code = envelope.error.code ?? code;
        message = envelope.error.message ?? message;
      }
    } catch {
      // Non-JSON error body; keep the status-text fallback.
    }
    throw new ApiError(response.status, code, message);
  }
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  /** Probe without a browser token, including one retained from an old tab session. */
  probeTransport: () => request<DaemonInfo>('/api/v1/daemon', { token: '', revokeOn401: false }),
  daemon: (signal?: AbortSignal) => request<DaemonInfo>('/api/v1/daemon', { signal }),

  reload: () => request<{ reloaded: boolean }>('/api/v1/daemon/reload', { method: 'POST' }),

  listJobs: (signal?: AbortSignal) => request<{ items: Definition[] }>('/api/v1/jobs', { signal }),

  workerStates: (signal?: AbortSignal) =>
    request<{ items: Record<string, WorkerState> }>('/api/v1/workers/states', { signal }),

  listAlertChannels: () => request<{ items: AlertChannel[] }>('/api/v1/alert-channels'),

  testAlertChannel: (name: string) => request<{ sent: boolean }>(`/api/v1/alert-channels/${encodeURIComponent(name)}/test`, { method: 'POST' }),

  alertMetrics: () => request<{ queue_depth: number; counts: Record<string, number> }>('/api/v1/metrics/alerts'),

  getJob: (name: string) => request<JobDetail>(`/api/v1/jobs/${encodeURIComponent(name)}`),

  createJob: (definition: Definition) =>
    request<Definition>('/api/v1/jobs', { method: 'POST', body: definition }),

  updateJob: (name: string, definition: Definition, revision?: number) =>
    request<Definition>(`/api/v1/jobs/${encodeURIComponent(name)}`, {
      method: 'PUT',
      body: definition,
      headers: revision ? { 'If-Match': String(revision) } : undefined,
    }),

  deleteJob: (name: string) =>
    request<{ deleted: boolean }>(`/api/v1/jobs/${encodeURIComponent(name)}`, { method: 'DELETE' }),

  triggerJob: (name: string) =>
    request<Run>(`/api/v1/jobs/${encodeURIComponent(name)}/trigger`, { method: 'POST' }),

  setJobEnabled: (name: string, enabled: boolean) =>
    request<{ enabled: boolean }>(`/api/v1/jobs/${encodeURIComponent(name)}/${enabled ? 'enable' : 'disable'}`, {
      method: 'POST',
    }),

  workerAction: (name: string, action: 'start' | 'stop' | 'restart') =>
    request<Record<string, boolean>>(`/api/v1/workers/${encodeURIComponent(name)}/${action}`, { method: 'POST' }),

  runMetrics: (range = '1h', buckets = 48, signal?: AbortSignal) =>
    request<RunMetrics>(`/api/v1/metrics/runs?range=${encodeURIComponent(range)}&buckets=${buckets}`, { signal }),

  listRuns: (job = '', limit = 50, signal?: AbortSignal) => {
    const query = new URLSearchParams({ limit: String(limit) });
    if (job) query.set('job', job);
    return request<{ items: Run[] }>(`/api/v1/runs?${query.toString()}`, { signal });
  },

  listRunsPage: (job: string, limit: number, before = '', filter = '', signal?: AbortSignal) => {
    const query = new URLSearchParams({ limit: String(limit) });
    if (job) query.set('job', job);
    if (before) query.set('before', before);
    if (filter) query.set('filter', filter);
    return request<{ items: Run[]; next_before: string }>(`/api/v1/runs?${query.toString()}`, { signal });
  },

  getRun: (id: string) => request<Run>(`/api/v1/runs/${encodeURIComponent(id)}`),

  runAlerts: (id: string) => request<{ items: RunAlert[] }>(`/api/v1/runs/${encodeURIComponent(id)}/alerts`),

  stopRun: (id: string) =>
    request<{ stopping: boolean }>(`/api/v1/runs/${encodeURIComponent(id)}/stop`, { method: 'POST' }),

  rawLogUrl: (id: string) => `/api/v1/runs/${encodeURIComponent(id)}/log/raw`,

  /** Windowed backlog read of stored log frames. */
  logFrames: (id: string, after = 0, limit = 5000, signal?: AbortSignal) =>
    request<{ items: Frame[] }>(`/api/v1/runs/${encodeURIComponent(id)}/log?after=${after}&limit=${limit}`, { signal }),

  /** Rotation is local-only in the daemon; TCP callers get 403. */
  rotateToken: () =>
    request<{ token: string; fingerprint: string }>('/api/v1/token/rotate', { method: 'POST' }),

  importPreview: (content: string) =>
    request<{ content_hash: string; definitions: Definition[] }>('/api/v1/import/preview', {
      method: 'POST',
      body: { content },
    }),

  importApply: (content: string, hash: string) =>
    request<{ content_hash: string; applied: number }>('/api/v1/import/apply', {
      method: 'POST',
      body: { content, hash },
    }),


  /** Validate a candidate token without mutating the stored session token. */
  validateToken: (token: string) => request<DaemonInfo>('/api/v1/daemon', { token, revokeOn401: false }),

  /** Authenticated binary download of an export bundle or raw log. */
  download: async (url: string): Promise<Blob> => {
    const headers: Record<string, string> = {};
    if (auth.token) headers.Authorization = `Bearer ${auth.token}`;
    const response = await fetch(publicPath(url), { headers });
    if (!response.ok) {
      if (response.status === 401) auth.revoke();
      throw new ApiError(response.status, 'download_failed', 'download failed');
    }
    return response.blob();
  },
};

export type LogStreamEvent =
  | { type: 'line'; frame: Frame }
  | { type: 'backlog_done' }
  | { type: 'done' }
  | { type: 'dropped' };

/**
 * Consume the resumable SSE log stream. Events: line frames in daemon
 * ingestion order, backlog_done (transition to live tailing), done (run ended
 * and stream closed), dropped (frames were removed by retention).
 */
export async function streamRunLogs(
  runId: string,
  after: number,
  onEvent: (event: LogStreamEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const headers: Record<string, string> = {};
  if (auth.token) headers.Authorization = `Bearer ${auth.token}`;
  const response = await fetch(publicPath(`/api/v1/runs/${encodeURIComponent(runId)}/log/stream?after=${after}`), {
    headers,
    signal,
  });
  if (!response.ok || !response.body) {
    if (response.status === 401) auth.revoke();
    let message = response.statusText || 'log stream failed';
    try {
      const envelope = (await response.json()) as { error?: { message?: string } };
      if (envelope.error?.message) message = envelope.error.message;
    } catch {
      // Keep fallback message.
    }
    throw new ApiError(response.status, 'stream_failed', message);
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  // The daemon accepts log payloads up to 16 MiB; base64 expands those frames.
  const MAX_EVENT_BYTES = 32 * 1024 * 1024;

  function handleBlock(block: string) {
    let event = '';
    const data: string[] = [];
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue; // heartbeat comment
      if (line.startsWith('event:')) event = line.slice(6).trim();
      if (line.startsWith('data:')) data.push(line.slice(5).trimStart());
    }
    switch (event) {
      case 'line': {
        if (data.length === 0) return;
        let frame: Frame;
        try {
          frame = JSON.parse(data.join('\n')) as Frame;
        } catch {
          // Ignore malformed frames; sequence accounting stays server-driven.
          return;
        }
        if (!frame || !Number.isSafeInteger(frame.sequence) || typeof frame.timestamp !== 'string' ||
            typeof frame.stream !== 'number' || typeof frame.payload !== 'string') return;
        onEvent({ type: 'line', frame });
        return;
      }
      case 'backlog_done':
        onEvent({ type: 'backlog_done' });
        return;
      case 'done':
        onEvent({ type: 'done' });
        return;
      case 'dropped':
        onEvent({ type: 'dropped' });
        return;
    }
  }

  try {
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true }).replace(/\r/g, '');
      let split: number;
      while ((split = buffer.indexOf('\n\n')) >= 0) {
        if (split > MAX_EVENT_BYTES) throw new Error('Log stream event is too large');
        handleBlock(buffer.slice(0, split));
        buffer = buffer.slice(split + 2);
      }
      if (buffer.length > MAX_EVENT_BYTES) throw new Error('Log stream event is too large');
    }
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}
