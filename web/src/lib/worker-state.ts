import type { WorkerState } from '../types';

export function workerStatusLabel(state: WorkerState): string {
  if (state.status === 'exhausted') return state.exhaustion_pending ? 'Exhausted (persistence pending)' : 'Exhausted';
  if (state.held) return 'On hold';
  if (state.active) return 'Running';
  if (state.status === 'restarting') return 'Restarting';
  return 'Stopped';
}
