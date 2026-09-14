import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link, useLocation } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon, type IconName } from './Icon';
import { auth } from '../auth';
import { daemonQuery, jobsQuery, runsQuery, RECENT_RUNS_LIMIT } from '../queries';
import { RANGE_LABEL, useRange, type RangeKey } from '../lib/range';
import { shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';

function currentTheme(): 'light' | 'dark' {
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
}

function ThemeToggle() {
  const [theme, setTheme] = useState(currentTheme);
  const toggle = () => {
    const next = theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    localStorage.setItem('minicron_theme', next);
    setTheme(next);
  };
  return (
    <button type="button" className="btn-icon" onClick={toggle} aria-label="Toggle color theme" title="Toggle theme">
      <Icon name={theme === 'dark' ? 'sun' : 'moon'} size={16} />
    </button>
  );
}

const NAV: { href: string; label: string; icon: IconName; match: (path: string) => boolean }[] = [
  { href: '/', label: 'Overview', icon: 'home', match: p => p === '/' },
  { href: '/jobs', label: 'Jobs & Workers', icon: 'list', match: p => p.startsWith('/jobs') },
  { href: '/runs', label: 'Runs', icon: 'clock', match: p => p.startsWith('/runs') },
  { href: '/metrics', label: 'Metrics', icon: 'trending-up', match: p => p === '/metrics' },
  { href: '/settings', label: 'Settings', icon: 'settings', match: p => p === '/settings' },
];

function NavLinks() {
  const [location] = useLocation();
  return (
    <nav className="top-nav" aria-label="Primary">
      {NAV.map(item => (
        <Link
          key={item.href}
          href={item.href}
          className={`nav-link ${item.match(location) ? 'active' : ''}`}
          title={item.label}
        >
          <Icon name={item.icon} size={15} />
          <span className="nav-label">{item.label}</span>
        </Link>
      ))}
    </nav>
  );
}

function GlobalSearch() {
  const [location, navigate] = useLocation();
  const inputRef = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const jobs = useQuery(jobsQuery());
  const runs = useQuery(runsQuery('', RECENT_RUNS_LIMIT));

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        inputRef.current?.focus();
        setOpen(true);
      }
      if (event.key === 'Escape') setOpen(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  useEffect(() => {
    setOpen(false);
    setQuery('');
  }, [location]);

  const q = query.trim().toLowerCase();
  const jobHits = (jobs.data ?? []).filter(job => job.name.toLowerCase().includes(q)).slice(0, 5);
  const runHits = q
    ? (runs.data ?? [])
        .filter(run => run.run_id.toLowerCase().includes(q) || run.job.toLowerCase().includes(q))
        .slice(0, 5)
    : [];

  const go = (path: string) => {
    setOpen(false);
    setQuery('');
    void navigate(path);
  };

  return (
    <div className="relative min-w-0 flex-1 max-w-md">
      <div className="flex items-center gap-2 rounded-lg border border-base-300 bg-base-100 px-3 py-1.5">
        <Icon name="search" size={15} className="shrink-0 faint" />
        <input
          ref={inputRef}
          value={query}
          onChange={event => {
            setQuery(event.target.value);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onBlur={() => setTimeout(() => setOpen(false), 150)}
          placeholder="Search jobs, workers, runs…"
          className="w-full bg-transparent text-sm outline-none placeholder:text-[color-mix(in_srgb,var(--color-base-content)_38%,transparent)]"
          aria-label="Search"
        />
        <span className="kbd">⌘K</span>
      </div>
      {open && q && (jobHits.length > 0 || runHits.length > 0) && (
        <div className="menu-pop left-0 right-0 top-full mt-1.5">
          {jobHits.length > 0 && (
            <div className="px-2 pb-1 pt-1.5 text-[0.68rem] font-semibold uppercase tracking-wide faint">Definitions</div>
          )}
          {jobHits.map(job => (
            <button
              key={job.name}
              type="button"
              className="menu-item"
              onMouseDown={event => {
                event.preventDefault();
                go(jobPath(job.name));
              }}
            >
              <Icon name={job.kind === 'worker' ? 'terminal' : 'calendar'} size={14} className="muted" />
              <span className="font-mono text-[0.8rem]">{job.name}</span>
            </button>
          ))}
          {runHits.length > 0 && (
            <div className="px-2 pb-1 pt-1.5 text-[0.68rem] font-semibold uppercase tracking-wide faint">Runs</div>
          )}
          {runHits.map(run => (
            <button
              key={run.run_id}
              type="button"
              className="menu-item"
              onMouseDown={event => {
                event.preventDefault();
                go(`/runs/${run.run_id}`);
              }}
            >
              <Icon name="history" size={14} className="muted" />
              <span className="font-mono text-[0.8rem]">#{shortRunId(run.run_id)}</span>
              <span className="muted ml-auto text-xs">{run.job}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function LivePill() {
  const daemon = useQuery(daemonQuery());
  const ok = !daemon.isError;
  const version = daemon.data ? ` · v${daemon.data.version}` : '';
  return (
    <span
      className="chip chip-success"
      title={ok ? `Daemon healthy${version} · lists auto-refresh (SSE)` : 'Cannot reach the daemon API'}
    >
      <span className={`dot ${ok ? 'dot-green dot-pulse' : 'dot-red'}`} />
      {ok ? 'Live' : 'Offline'}
    </span>
  );
}

function RangeSelect() {
  const { range, setRange } = useRange();
  return (
    <label className="relative inline-flex items-center">
      <span className="pointer-events-none absolute left-2.5 faint">
        <Icon name="calendar" size={14} />
      </span>
      <select
        value={range}
        onChange={event => setRange(event.target.value as RangeKey)}
        aria-label="Time range"
        className="mc-select !w-auto !rounded-lg !py-1.5 !pl-8 !pr-8 !text-[0.8125rem]"
      >
        {(Object.keys(RANGE_LABEL) as RangeKey[]).map(key => (
          <option key={key} value={key}>
            {RANGE_LABEL[key]}
          </option>
        ))}
      </select>
    </label>
  );
}

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="app-shell">
      <header className="topbar">
        <Link href="/" className="brand">
          <span className="brand-mark">
            <Icon name="terminal" size={14} strokeWidth={2.4} />
          </span>
          <span className="brand-name">minicron</span>
        </Link>
        <NavLinks />
        <GlobalSearch />
        <div className="topbar-actions">
          <LivePill />
          <RangeSelect />
          <ThemeToggle />
          <button
            type="button"
            className="btn-icon"
            onClick={() => auth.logout()}
            aria-label="Sign out"
            title="Sign out (clears the token from this tab)"
          >
            <Icon name="log-out" size={16} />
          </button>
        </div>
      </header>
      <main className="page">{children}</main>
    </div>
  );
}

/** Page heading block: title + optional subtitle + right-side actions. */
export function PageHeader({
  title,
  subtitle,
  badge,
  actions,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  badge?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
      <div className="min-w-0">
        <div className="flex items-center gap-2.5">
          <h1 className="text-[1.45rem] font-bold tracking-tight">{title}</h1>
          {badge}
        </div>
        {subtitle && <p className="mt-0.5 text-sm muted">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
