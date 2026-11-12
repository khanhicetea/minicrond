import type { WorkerState } from '../types';

/** Short label for a worker that needs attention (badge text). */
export function workerAttentionLabel(state: WorkerState): string {
  if (state.status === 'exhausted') return 'Exhausted';
  if (state.held) return 'On hold';
  if (state.status === 'restarting') return 'Restarting';
  return state.failures > 0 ? 'Failing' : workerStatusLabel(state);
}

export function workerStatusLabel(state: WorkerState): string {
  if (state.status === 'exhausted') return state.exhaustion_pending ? 'Exhausted (persistence pending)' : 'Exhausted';
  if (state.held) return 'On hold';
  if (state.active) return 'Running';
  if (state.status === 'restarting') return 'Restarting';
  return 'Stopped';
}
