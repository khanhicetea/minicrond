import { useState, type FormEvent } from 'react';
import { useLocation } from 'wouter';
import { Icon } from '../components/Icon';
import { api, errorText } from '../api';
import { auth } from '../auth';

export default function Login() {
  const [, navigate] = useLocation();
  const [token, setToken] = useState('');
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const candidate = token.trim();
    if (!candidate || busy) return;
    setBusy(true);
    setError('');
    try {
      await api.validateToken(candidate);
      auth.login(candidate);
      navigate('/');
    } catch (err) {
      setError(
        err instanceof Error && err.message.includes('Failed to fetch')
          ? 'Could not reach the daemon. Is it running?'
          : errorText(err),
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-base-200 px-4">
      <div className="w-full max-w-md">
        <div className="panel p-6 sm:p-8">
          <div className="mb-5 flex items-center gap-2.5">
            <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-blue-500/15 text-blue-400">
              <Icon name="terminal" size={18} strokeWidth={2.4} />
            </span>
            <div>
              <h1 className="text-xl font-bold tracking-tight">minicron</h1>
              <p className="text-xs muted">Local cron scheduler &amp; process supervisor</p>
            </div>
          </div>
          <p className="mb-4 text-sm muted">
            Enter the daemon bearer token to continue. The daemon prints it once on first start; it is kept in this
            tab's session storage only.
          </p>
          <form onSubmit={handleSubmit} className="space-y-3">
            <div>
              <label htmlFor="token" className="field-label">Bearer token</label>
              <div className="relative">
                <input
                  id="token"
                  type={reveal ? 'text' : 'password'}
                  autoComplete="off"
                  autoFocus
                  required
                  value={token}
                  onChange={event => setToken(event.target.value)}
                  className="mc-input mono !pr-10"
                  placeholder="paste token"
                  spellCheck={false}
                />
                <button
                  type="button"
                  className="btn-icon absolute right-1 top-1/2 -translate-y-1/2"
                  onClick={() => setReveal(value => !value)}
                  aria-label={reveal ? 'Hide token' : 'Show token'}
                >
                  <Icon name="eye" size={15} />
                </button>
              </div>
            </div>
            {error && (
              <div role="alert" className="rounded-lg border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-300">
                {error}
              </div>
            )}
            <button type="submit" disabled={busy || !token.trim()} className="btn-primary-x w-full !py-2">
              {busy && <Icon name="loader" size={14} className="spin" />}
              Sign in
            </button>
          </form>
          <p className="mt-4 text-xs faint">
            Lost the token? Rotate it locally with <code className="font-mono">minicrond token --rotate</code> (requires
            the Unix socket).
          </p>
        </div>
        <p className="mt-4 text-center text-xs faint">minicron · local-first job scheduling</p>
      </div>
    </div>
  );
}
