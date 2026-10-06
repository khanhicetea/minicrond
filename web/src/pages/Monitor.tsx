import { useEffect, useRef, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'wouter';
import { api, errorText } from '../api';
import { Icon } from '../components/Icon';
import { cpuPercent, memoryMiB } from '../lib/resources';
import { shortRunId, formatTimestamp } from '../lib/format';
import type { MonitorSnapshot } from '../types';

export default function Monitor() {
  const [visible, setVisible] = useState(() => !document.hidden);
  useEffect(() => {
    const changed = () => setVisible(!document.hidden);
    document.addEventListener('visibilitychange', changed);
    return () => document.removeEventListener('visibilitychange', changed);
  }, []);
  const query = useQuery({
    queryKey: ['monitor'],
    queryFn: ({ signal }) => api.monitor(signal),
    enabled: visible,
    refetchInterval: 3000,
    refetchIntervalInBackground: false,
    refetchOnWindowFocus: false,
    retry: false,
    gcTime: 0,
  });
  const previous = useRef<MonitorSnapshot | undefined>(undefined);
  const [percentages, setPercentages] = useState<Record<string, number | undefined>>({});
  useEffect(() => {
    const current = query.data;
    if (!current) return;
    const old = previous.current;
    const oldRuns = new Map(old?.items.map(item => [item.run_id, item.stats]));
    const next: Record<string, number | undefined> = { daemon: cpuPercent(old?.daemon, current.daemon) };
    for (const item of current.items) next[item.run_id] = cpuPercent(oldRuns.get(item.run_id), item.stats);
    setPercentages(next);
    previous.current = current;
  }, [query.data]);

  const data = query.data;
  const percent = (key: string) => percentages[key] == null ? '—' : `${percentages[key]!.toFixed(1)}%`;
  return <div className="space-y-4">
    <header className="flex flex-wrap items-center justify-between gap-3">
      <div className="flex items-center gap-3">
        <span className="icon-badge icon-blue h-10 w-10"><Icon name="activity" size={20} /></span>
        <div>
          <h1 className="text-xl font-bold">Monitor</h1>
          <p className="mt-0.5 text-xs muted">Live process usage</p>
        </div>
      </div>
      <div className="flex items-center gap-3 text-xs">
        {data && <span className="muted" title="Last sample">{formatTimestamp(data.sampled_at)}</span>}
        <span className={`chip ${query.isError ? 'chip-warn' : visible ? 'chip-success' : 'chip-neutral'}`} title="Refreshes every 3 seconds while visible; no sampling with no viewers.">
          <Icon name={query.isError ? 'alert-circle' : visible ? 'activity' : 'pause'} size={12} />
          {query.isError ? 'Stale' : visible ? 'Live · 3s' : 'Paused'}
        </span>
      </div>
    </header>
    {query.isError && <div role="alert" className="panel px-4 py-3 text-sm text-amber-400">
      Monitoring could not be refreshed: {errorText(query.error)}. {data && 'Displayed values are stale.'}
      <button className="ml-3 underline" type="button" onClick={() => void query.refetch()}>Retry</button>
    </div>}
    {query.isPending && <div className="skeleton h-48 w-full" />}
    {data && <>
      {!data.supported && <p className="text-sm text-amber-400">Live CPU/RSS monitoring requires Linux procfs. Exit accounting is still saved for completed runs.</p>}
      <section className="panel overflow-x-auto">
        <table className="mc-table">
          <thead><tr><th>Process</th><th>Kind</th><th>PID</th><th title="Interval CPU usage; 100% equals one logical CPU. Requires two samples."><span className="inline-flex items-center gap-1.5"><Icon name="activity" size={13} />CPU</span></th><th title="Resident memory of the direct process, excluding subprocesses."><span className="inline-flex items-center gap-1.5"><Icon name="database" size={13} />RSS</span></th><th title="Accumulated CPU time of the direct process."><span className="inline-flex items-center gap-1.5"><Icon name="clock" size={13} />CPU time</span></th></tr></thead>
          <tbody>
            <tr className="bg-base-200/50"><td className="font-semibold"><span className="inline-flex items-center gap-2"><Icon name="settings" size={14} className="text-sky-400" />minicrond</span></td><td><span className="chip chip-info !py-0.5">daemon</span></td><td>{data.daemon_pid}</td><td>{percent('daemon')}</td><td>{memoryMiB(data.daemon?.rss_bytes)}</td><td>{data.daemon ? `${(data.daemon.cpu_us / 1e6).toFixed(3)} s` : '—'}</td></tr>
            {data.items.map(item => <tr key={item.run_id}>
              <td><Link className="inline-flex items-center gap-2 font-medium text-sky-300 hover:text-sky-200" href={`/runs/${item.run_id}`}><Icon name={item.kind === 'worker' ? 'terminal' : 'calendar'} size={14} />{item.job}</Link><span className="ml-2 text-xs faint">#{shortRunId(item.run_id)}</span></td>
              <td>{item.kind}</td><td>{item.pid || '—'}</td><td>{percent(item.run_id)}</td><td>{memoryMiB(item.stats?.rss_bytes)}</td><td>{item.stats ? `${(item.stats.cpu_us / 1e6).toFixed(3)} s` : '—'}</td>
            </tr>)}
          </tbody>
        </table>
      </section>
      {data.active === 0 && <div className="flex items-center justify-center gap-2 py-4 text-sm muted"><Icon name="moon" size={16} />No active runs</div>}
      {data.truncated && <p role="status" className="text-sm text-amber-400">Showing a bounded subset of {data.items.length} of {data.active} active runs.</p>}
      <div className="flex items-center gap-1.5 text-xs faint" title="Job/worker values exclude subprocesses. — means unavailable or waiting for a second sample. Live samples are not saved; completed runs store kernel exit accounting.">
        <Icon name="info" size={13} />Direct processes only · — unavailable
      </div>
    </>}
  </div>;
}
