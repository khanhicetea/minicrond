import { useQuery } from '@tanstack/react-query';
import { jobsQuery, runsQuery, RECENT_RUNS_LIMIT, useWorkerStates } from '../queries';
import { isFailedStatus } from '../components/ui/status';
import type { Definition, Run, WorkerState } from '../types';

export interface AttentionItem {
  definition: Definition;
  /** Latest run for a failed job. */
  run?: Run;
  /** Worker runtime state for a failing or held worker. */
  worker?: WorkerState;
}

/** A worker needs attention when it has recent failures or is held. */
export function workerNeedsAttention(state: WorkerState | undefined): boolean {
  return (state?.failures ?? 0) > 0 || state?.held === true;
}

/**
 * Shared overview data: definitions, the global recent-runs feed grouped per
 * definition (newest first), worker states, and the "needs attention" list.
 * The header badge and the Jobs page read the same react-query cache entries,
 * so this adds no requests beyond what those views already made.
 */
export function useOverview(options: { poll?: boolean } = {}) {
  const jobs = useQuery({ ...jobsQuery(), ...(options.poll ? { refetchInterval: 15_000, refetchIntervalInBackground: false } : {}) });
  const runs = useQuery({ ...runsQuery('', RECENT_RUNS_LIMIT), ...(options.poll ? { refetchInterval: 5_000, refetchIntervalInBackground: false } : {}) });
  const definitions = jobs.data ?? [];
  const workerStates = useWorkerStates(definitions.filter(definition => definition.kind === 'worker').map(definition => definition.name));

  const runsByJob = new Map<string, Run[]>();
  for (const run of runs.data ?? []) {
    const list = runsByJob.get(run.job);
    if (list) list.push(run);
    else runsByJob.set(run.job, [run]);
  }

  const attention: AttentionItem[] = [];
  for (const definition of definitions) {
    if (definition.enabled === false) continue;
    if (definition.kind === 'worker') {
      const worker = workerStates[definition.name];
      if (workerNeedsAttention(worker)) attention.push({ definition, worker });
    } else {
      const run = runsByJob.get(definition.name)?.[0];
      if (run && isFailedStatus(run.status)) attention.push({ definition, run });
    }
  }

  return { jobs, runs, definitions, runsByJob, workerStates, attention };
}
