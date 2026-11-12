import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Link, useLocation } from 'wouter';
import { useQuery } from '@tanstack/react-query';
import { Icon } from './Icon';
import { IconButton, SearchField } from './ui';
import { auth, useAuthState } from '../auth';
import { daemonQuery } from '../queries';
import { useOverview } from '../lib/attention';
import { isEmbedded } from '../lib/embed';
import { formatUptime, shortRunId } from '../lib/format';
import { jobPath } from '../lib/routes';

export { PageHeader } from './ui';

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
  return <IconButton icon={theme === 'dark' ? 'sun' : 'moon'} label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'} onClick={toggle} />;
}

const NAV: { href: string; label: string; match: (path: string) => boolean; attention?: boolean }[] = [
  { href: '/jobs', label: 'Jobs & workers', match: p => p === '/' || p.startsWith('/jobs'), attention: true },
  { href: '/runs', label: 'Runs', match: p => p.startsWith('/runs') },
  { href: '/metrics', label: 'Metrics', match: p => p === '/metrics' },
  { href: '/monitor', label: 'Monitor', match: p => p === '/monitor' },
  { href: '/settings', label: 'Settings', match: p => p === '/settings' },
];

function NavTabs({ attentionCount }: { attentionCount: number }) {
  const [location] = useLocation();
  return (
    <nav className="nav-tabs" aria-label="Primary">
      {NAV.map(item => (
        <Link key={item.href} href={item.href} className="nav-tab" aria-current={item.match(location) ? 'page' : undefined}>
          {item.label}
          {item.attention && attentionCount > 0 && (
            <span className="nav-count" title={`${attentionCount} need attention`}>
              <span className="sr-only">, </span>
              {attentionCount}
              <span className="sr-only"> need attention</span>
            </span>
          )}
        </Link>
      ))}
    </nav>
  );
}

function GlobalSearch({ compact }: { compact: boolean }) {
  const [location, navigate] = useLocation();
  const inputRef = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState('');
  const [open, setOpen] = useState(false);
  const [expanded, setExpanded] = useState(!compact);
  const { definitions, runs } = useOverview();

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        setExpanded(true);
        requestAnimationFrame(() => inputRef.current?.focus());
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
    setExpanded(!compact);
  }, [location, compact]);

  const q = query.trim().toLowerCase();
  const jobHits = q ? definitions.filter(job => job.name.toLowerCase().includes(q)).slice(0, 6) : [];
  const runHits = q ? (runs.data ?? []).filter(run => run.run_id.toLowerCase().includes(q) || run.job.toLowerCase().includes(q)).slice(0, 5) : [];

  const go = (path: string) => {
    setOpen(false);
    setQuery('');
    void navigate(path);
  };

  if (!expanded) {
    return (
      <IconButton
        icon="search"
        label="Search jobs, workers and runs"
        outlined
        onClick={() => {
          setExpanded(true);
          requestAnimationFrame(() => inputRef.current?.focus());
        }}
      />
    );
  }

  return (
    <div
      className="relative"
      onBlur={event => {
        if (event.currentTarget.contains(event.relatedTarget as Node | null)) return;
        setOpen(false);
        if (compact && !query) setExpanded(false);
      }}
    >
      <SearchField
        ref={inputRef}
        value={query}
        onChange={value => {
          setQuery(value);
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        label="Search jobs, workers and runs"
        placeholder="Search jobs, runs"
        className={compact ? 'w-52' : 'w-60'}
        hint={compact ? undefined : <span className="kbd">⌘K</span>}
      />
      {open && q && (
        <div className="menu-pop right-0 top-full mt-1.5 w-80 max-w-[calc(100vw-2rem)]">
          {jobHits.length === 0 && runHits.length === 0 && <p className="px-2 py-2 text-sm muted">No matches.</p>}
          {jobHits.length > 0 && <div className="menu-label">Definitions</div>}
          {jobHits.map(job => (
            <button key={job.name} type="button" className="menu-item" onClick={() => go(jobPath(job.name))}>
              <Icon name={job.kind === 'worker' ? 'terminal' : 'calendar'} size={14} />
              <span className="mono text-[0.8rem]">{job.name}</span>
            </button>
          ))}
          {runHits.length > 0 && <div className="menu-label">Runs</div>}
          {runHits.map(run => (
            <button key={run.run_id} type="button" className="menu-item" onClick={() => go(`/runs/${encodeURIComponent(run.run_id)}`)}>
              <Icon name="history" size={14} />
              <span className="mono text-[0.8rem]">#{shortRunId(run.run_id)}</span>
              <span className="ml-auto text-xs muted">{run.job}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

function LivePill() {
  const daemon = useQuery(daemonQuery());
  const state = daemon.isSuccess ? 'ok' : daemon.isPending ? 'pending' : 'error';
  const title = daemon.data
    ? `Daemon healthy · v${daemon.data.version} · up ${formatUptime(daemon.data.uptime_s)}`
    : state === 'pending'
      ? 'Checking daemon connection'
      : 'Cannot reach the daemon API';
  return (
    <span className={`live-pill ${state}`} title={title}>
      <span className={`dot ${state === 'ok' ? 'dot-green' : state === 'pending' ? 'dot-gray' : 'dot-red'}`} />
      {state === 'ok' ? 'Live' : state === 'pending' ? 'Connecting' : 'Offline'}
      <span className="sr-only">{title}</span>
    </span>
  );
}

/** True below the phone breakpoint; the header then collapses search to an icon. */
function useNarrow(): boolean {
  const query = '(max-width: 720px)';
  const [narrow, setNarrow] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const media = window.matchMedia(query);
    const update = () => setNarrow(media.matches);
    media.addEventListener('change', update);
    return () => media.removeEventListener('change', update);
  }, []);
  return narrow;
}

/** Opens the current page as a top-level tab (embedded mode only). */
function openStandalone() {
  const url = new URL(window.location.href);
  url.searchParams.delete('embed');
  window.open(url.toString(), '_blank', 'noopener');
}

export default function Layout({ children }: { children: ReactNode }) {
  const { mode } = useAuthState();
  const [location] = useLocation();
  const { attention } = useOverview();
  const narrow = useNarrow();
  // The Runs workspace fills the area under the header edge to edge.
  const flush = location === '/runs';
  return (
    <div className="app-shell">
      <header className="app-header">
        <Link href="/jobs" className="brand" aria-label="minicrond home">
          <span className="brand-mark">
            <Icon name="clock" size={15} strokeWidth={2.4} />
          </span>
          {!isEmbedded && <span className="brand-name">minicrond</span>}
        </Link>
        <NavTabs attentionCount={attention.length} />
        <div className="header-actions">
          <GlobalSearch compact={isEmbedded || narrow} />
          <LivePill />
          <ThemeToggle />
          {isEmbedded && <IconButton icon="external-link" label="Open minicrond in a new tab" onClick={openStandalone} />}
          {mode === 'token' && <IconButton icon="log-out" label="Sign out" title="Sign out (clears the token from this tab)" onClick={() => auth.logout()} />}
        </div>
      </header>
      <main className={flush ? 'page-flush' : 'page'}>{children}</main>
    </div>
  );
}
