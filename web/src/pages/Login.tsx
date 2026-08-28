import { useState, type FormEvent } from 'react';
import { useLocation } from 'wouter';
import { api, errorText } from '../api';
import { auth } from '../auth';

export default function Login() {
  const [, navigate] = useLocation();
  const [token, setToken] = useState('');
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
      setError(err instanceof Error && err.message.includes('Failed to fetch')
        ? 'Could not reach the daemon. Is it running?'
        : errorText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-base-200 px-4">
      <div className="card w-full max-w-md border border-base-300 bg-base-100 shadow-xl">
        <div className="card-body gap-4">
          <h1 className="card-title text-2xl">
            <span aria-hidden>⏱</span> minicron
          </h1>
          <p className="text-sm text-base-content/70">
            Enter the daemon bearer token to continue. The daemon prints it once on first start; it is kept in this
            tab's session storage only.
          </p>
          <form onSubmit={handleSubmit} className="space-y-3">
            <label className="form-control">
              <span className="label label-text font-semibold">Bearer token</span>
              <input
                type="password"
                autoComplete="off"
                autoFocus
                required
                value={token}
                onChange={event => setToken(event.target.value)}
                className="input input-bordered w-full font-mono"
                placeholder="paste token"
              />
            </label>
            {error && (
              <div role="alert" className="alert alert-error text-sm">
                <span>{error}</span>
              </div>
            )}
            <button type="submit" disabled={busy || !token.trim()} className="btn btn-primary w-full">
              {busy && <span className="loading loading-spinner loading-xs" />}
              Sign in
            </button>
          </form>
          <p className="text-xs text-base-content/50">
            Lost the token? Rotate it locally with <code className="font-mono">minicron token --rotate</code> (requires
            the Unix socket).
          </p>
        </div>
      </div>
    </div>
  );
}
