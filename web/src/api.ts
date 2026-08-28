import { auth } from './auth';
import type { DaemonInfo, Definition, Frame, JobDetail, Run } from './types';

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
}

async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const token = options.token ?? auth.token;
  const headers: Record<string, string> = { ...options.headers };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  const response = await fetch(path, {
    method: options.method ?? 'GET',
    headers,
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    signal: options.signal,
  });
  if (!response.ok) {
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
  daemon: () => request<DaemonInfo>('/api/v1/daemon'),

  reload: () => request<{ reloaded: boolean }>('/api/v1/daemon/reload', { method: 'POST' }),

  listJobs: () => request<{ items: Definition[] }>('/api/v1/jobs'),

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

  listRuns: (job = '', limit = 50) => {
    const query = new URLSearchParams({ limit: String(limit) });
    if (job) query.set('job', job);
    return request<{ items: Run[] }>(`/api/v1/runs?${query.toString()}`);
  },

  getRun: (id: string) => request<Run>(`/api/v1/runs/${encodeURIComponent(id)}`),

  stopRun: (id: string) =>
    request<{ stopping: boolean }>(`/api/v1/runs/${encodeURIComponent(id)}/stop`, { method: 'POST' }),

  rawLogUrl: (id: string) => `/api/v1/runs/${encodeURIComponent(id)}/log/raw`,

  /** Windowed backlog read of stored log frames. */
  logFrames: (id: string, after = 0, limit = 5000) =>
    request<{ items: Frame[] }>(`/api/v1/runs/${encodeURIComponent(id)}/log?after=${after}&limit=${limit}`),

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
  validateToken: (token: string) => request<DaemonInfo>('/api/v1/daemon', { token }),

  /** Authenticated binary download of an export bundle or raw log. */
  download: async (url: string): Promise<Blob> => {
    const headers: Record<string, string> = {};
    if (auth.token) headers.Authorization = `Bearer ${auth.token}`;
    const response = await fetch(url, { headers });
    if (!response.ok) throw new ApiError(response.status, 'download_failed', 'download failed');
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
  const response = await fetch(`/api/v1/runs/${encodeURIComponent(runId)}/log/stream?after=${after}`, {
    headers,
    signal,
  });
  if (!response.ok || !response.body) {
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

  function handleBlock(block: string) {
    let event = '';
    for (const line of block.split('\n')) {
      if (line.startsWith(':')) continue; // heartbeat comment
      if (line.startsWith('event:')) event = line.slice(6).trim();
    }
    switch (event) {
      case 'line': {
        const dataLine = block.split('\n').find(l => l.startsWith('data:'));
        if (!dataLine) return;
        try {
          onEvent({ type: 'line', frame: JSON.parse(dataLine.slice(5).trim()) as Frame });
        } catch {
          // Ignore malformed frames; sequence accounting stays server-driven.
        }
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

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true }).replace(/\r/g, '');
    let split: number;
    while ((split = buffer.indexOf('\n\n')) >= 0) {
      handleBlock(buffer.slice(0, split));
      buffer = buffer.slice(split + 2);
    }
  }
}
