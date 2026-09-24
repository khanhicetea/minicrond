/** API response types; Definition and Run are generated from internal/model. */

import type { Definition, Run } from './generated/model';

export { KindJob, KindWorker } from './generated/model';
export type { Definition, Kind, Run } from './generated/model';

export interface DaemonInfo {
  version: string;
  schema_version: number;
  uptime_s: number;
  capabilities: string[];
  token_fingerprint: string;
}

export interface RunMetricBucket {
  success: number;
  failure: number;
  active: number;
  queued: number;
  duration_p50_ms?: number;
  duration_p95_ms?: number;
}

export interface RunJobMetrics {
  name: string;
  total: number;
  succeeded: number;
  failed: number;
  active: number;
  duration_p50_ms?: number;
  duration_p95_ms?: number;
}

export interface RunMetrics {
  total: number;
  succeeded: number;
  failed: number;
  active: number;
  queued: number;
  duration_p50_ms?: number;
  duration_p95_ms?: number;
  jobs: RunJobMetrics[];
  buckets: RunMetricBucket[];
}

export interface WorkerState {
  held: boolean;
  active: boolean;
  failures: number;
}

export interface AlertChannel {
  name: string;
  type: string;
  batch_window: number;
}

export interface RunAlert {
  channel: string;
  status: string;
  attempts: number;
  last_error?: string | null;
  updated_at?: string | null;
}

export interface JobDetail {
  definition: Definition;
  hash: string;
  active_runs: number;
  next_fire_at?: string;
  worker_state?: WorkerState;
}

/** Tagged log frame; payload is base64 (see logstore). */
export interface Frame {
  sequence: number;
  timestamp: string;
  stream: number; // 1 stdout, 2 stderr, 3 system
  flags: number;
  payload: string;
}

export const STREAM_STDERR = 2;
export const STREAM_SYSTEM = 3;

/** Run statuses that still need polling. */
export function isActiveRun(run: Pick<Run, 'status'>): boolean {
  return run.status === 'pending' || run.status === 'running';
}

export function isTerminalStatus(status: string): boolean {
  return ['succeeded', 'failed', 'timeout', 'stopped', 'interrupted', 'skipped', 'missed'].includes(status);
}
