/** Types mirroring the JSON wire contract of the minicron API (see model). */

export type Kind = 'job' | 'worker';

export interface Definition {
  definition_id?: number;
  name: string;
  kind: Kind;
  authority?: string;
  revision?: number;
  source_file?: string;
  enabled?: boolean;
  command?: string;
  argv?: string[];
  shell?: string;
  schedule?: string;
  timezone?: string;
  /** Scheduler-owned next fire time, returned by the jobs API. */
  next_fire_at?: string;
  catch_up?: string;
  on_overlap?: string;
  run_on_start?: boolean;
  run_as?: string;
  working_dir?: string;
  env_base?: string;
  env?: Record<string, string>;
  secret_env?: Record<string, string>;
  env_file?: string;
  timeout?: string;
  grace?: string;
  stop_signal?: string;
  success_codes?: number[];
  keep_runs?: number;
  keep_for?: string;
  log_max?: string;
  log_on_full?: string;
  labels?: Record<string, string>;
  autostart?: boolean;
  restart?: string;
  restart_delay?: string;
  max_restart_attempts?: number;
  healthy_after?: string;
  priority?: number;
}

export interface Run {
  run_id: string;
  definition_id: number;
  job: string;
  kind: string;
  revision: number;
  definition_hash: string;
  status: string;
  end_reason?: string;
  trigger: string;
  attempt: number;
  scheduled_for?: string;
  missed_count?: number;
  boot_id?: string;
  pid?: number;
  pgid?: number;
  process_start_id?: string;
  exit_code?: number;
  signal?: string;
  queued_at: string;
  started_at?: string;
  ended_at?: string;
  log_ref?: string;
  log_bytes: number;
  log_truncated: boolean;
}

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
