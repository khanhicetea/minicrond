import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from './api';
import type { Definition } from './types';
import { isActiveRun } from './types';

export const keys = {
  daemon: ['daemon'] as const,
  jobs: ['jobs'] as const,
  job: (name: string) => ['job', name] as const,
  runs: (job: string, limit: number) => ['runs', job, limit] as const,
  run: (id: string) => ['run', id] as const,
};

export const daemonQuery = () =>
  queryOptions({ queryKey: keys.daemon, queryFn: api.daemon, staleTime: 15_000 });

export const jobsQuery = () =>
  queryOptions({ queryKey: keys.jobs, queryFn: api.listJobs, select: data => data.items });

export const jobQuery = (name: string) => queryOptions({ queryKey: keys.job(name), queryFn: () => api.getJob(name) });

export const runsQuery = (job = '', limit = 50) =>
  queryOptions({
    queryKey: keys.runs(job, limit),
    queryFn: () => api.listRuns(job, limit),
    select: data => data.items,
    refetchInterval: query => (query.state.data?.items.some(isActiveRun) ? 4_000 : false),
  });

/** Poll the run detail only while it can still change. */
export const runQuery = (id: string) =>
  queryOptions({
    queryKey: keys.run(id),
    queryFn: () => api.getRun(id),
    refetchInterval: query => (query.state.data && isActiveRun(query.state.data) ? 3_000 : false),
  });

function invalidateRuns(client: ReturnType<typeof useQueryClient>) {
  void client.invalidateQueries({ queryKey: ['runs'] });
  void client.invalidateQueries({ queryKey: ['run'] });
}

export const useReloadDaemon = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: api.reload,
    onSuccess: () => void client.invalidateQueries(),
  });
};

export const useTriggerJob = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.triggerJob(name),
    onSuccess: () => invalidateRuns(client),
  });
};

export const useSetJobEnabled = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) => api.setJobEnabled(name, enabled),
    onSuccess: (_data, variables) => {
      void client.invalidateQueries({ queryKey: ['jobs'] });
      void client.invalidateQueries({ queryKey: keys.job(variables.name) });
    },
  });
};

export const useSaveJob = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ name, definition, revision }: { name?: string; definition: Definition; revision?: number }) =>
      name ? api.updateJob(name, definition, revision) : api.createJob(definition),
    onSuccess: saved => {
      void client.invalidateQueries({ queryKey: ['jobs'] });
      void client.invalidateQueries({ queryKey: keys.job(saved.name) });
    },
  });
};

export const useDeleteJob = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteJob(name),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['jobs'] });
      invalidateRuns(client);
    },
  });
};

export const useWorkerAction = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: ({ name, action }: { name: string; action: 'start' | 'stop' | 'restart' }) =>
      api.workerAction(name, action),
    onSuccess: (_data, variables) => {
      void client.invalidateQueries({ queryKey: keys.job(variables.name) });
      invalidateRuns(client);
    },
  });
};

export const useStopRun = () => {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.stopRun(id),
    onSuccess: () => invalidateRuns(client),
  });
};
