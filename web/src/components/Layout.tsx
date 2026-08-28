import { useState, type ReactNode } from 'react';
import { Link } from 'wouter';
import { auth } from '../auth';

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
    <button type="button" className="btn btn-ghost btn-sm" onClick={toggle} aria-label="Toggle color theme">
      {theme === 'dark' ? '☀️' : '🌙'}
    </button>
  );
}

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen bg-base-200">
      <header className="navbar sticky top-0 z-20 border-b border-base-300 bg-base-100 px-2 shadow-sm sm:px-4">
        <div className="flex flex-1 items-center gap-1">
          <Link href="/" className="btn btn-ghost btn-sm px-2 text-lg font-bold">
            <span aria-hidden>⏱</span> minicron
          </Link>
        </div>
        <nav className="flex items-center gap-1">
          <Link href="/" className="btn btn-ghost btn-sm">
            Dashboard
          </Link>
          <Link href="/jobs/new" className="btn btn-ghost btn-sm">
            New definition
          </Link>
          <ThemeToggle />
          <button type="button" className="btn btn-outline btn-sm" onClick={() => auth.logout()}>
            Log out
          </button>
        </nav>
      </header>
      <main className="mx-auto w-full max-w-6xl px-3 py-6 sm:px-4">{children}</main>
    </div>
  );
}
