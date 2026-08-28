import { useEffect, useMemo, useState } from 'react';
import { useSearch } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from '../components/Icon';
import { PageHeader } from '../components/Layout';
import RunsTable from '../components/RunsTable';
import { runsQuery } from '../queries';

type FilterKey = 'all' | 'failed' | 'scheduled' | 'manual';

const FILTERS: { key: FilterKey; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'failed', label: 'Failed' },
  { key: 'scheduled', label: 'Scheduled' },
  { key: 'manual', label: 'Manual' },
];

/** Full run history with quick filters (design's runs vocabulary). */
export default function Runs() {
  const initial = (new URLSearchParams(useSearch()).get('filter') as FilterKey) || 'all';
  const [filter, setFilter] = useState<FilterKey>(initial);
  const [query, setQuery] = useState('');
  useEffect(() => setFilter(initial), [initial]);

  const runs = useQuery(runsQuery('', 200));
  const items = useMemo(() => {
    const q = query.trim().toLowerCase();
    return (runs.data ?? []).filter(run => {
      if (q && !run.job.toLowerCase().includes(q) && !run.run_id.toLowerCase().includes(q)) return false;
      switch (filter) {
        case 'failed':
          return ['failed', 'timeout', 'interrupted'].includes(run.status);
        case 'scheduled':
          return run.trigger === 'schedule';
        case 'manual':
          return run.trigger === 'manual';
        default:
          return true;
      }
    });
  }, [runs.data, filter, query]);

  return (
    <div className="space-y-4">
      <PageHeader title="Runs" subtitle="Run history across all jobs and workers." />

      <div className="flex flex-wrap items-center gap-2">
        <div className="flex items-center gap-1.5">
          {FILTERS.map(f => (
            <button key={f.key} type="button" className={`pill ${filter === f.key ? 'active' : ''}`} onClick={() => setFilter(f.key)}>
              {f.key === 'failed' && <span className="dot dot-red" />}
              {f.label}
            </button>
          ))}
        </div>
        <div className="ml-auto flex min-w-[12rem] items-center gap-2 rounded-lg border border-base-300 bg-base-100 px-3 py-1.5 sm:max-w-xs">
          <Icon name="search" size={14} className="faint shrink-0" />
          <input
            value={query}
            onChange={event => setQuery(event.target.value)}
            placeholder="Filter by job or run id"
            className="w-full bg-transparent text-sm outline-none placeholder:text-[color-mix(in_srgb,var(--color-base-content)_35%,transparent)]"
            aria-label="Filter runs"
          />
        </div>
      </div>

      {runs.isPending ? <div className="skeleton h-64 w-full" /> : <RunsTable runs={items} />}
    </div>
  );
}
