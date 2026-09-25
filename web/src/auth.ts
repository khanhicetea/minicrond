import { useSyncExternalStore } from 'react';

type AuthMode = 'checking' | 'login' | 'token' | 'proxy' | 'revoked' | 'error' | 'unsupported';
type AuthState = { mode: AuthMode };

const STORAGE_KEY = 'minicron_token';
let storedToken = sessionStorage.getItem(STORAGE_KEY) ?? '';
let state: AuthState = { mode: 'checking' };
const serverState: AuthState = { mode: 'checking' };
const listeners = new Set<() => void>();

function setMode(mode: AuthMode) {
  state = { mode };
  for (const listener of listeners) listener();
}

export const auth = {
  get mode() { return state.mode; },
  // Never send a retained token while probing or using a proxy session.
  get token() { return state.mode === 'token' ? storedToken : ''; },
  useProxy: () => {
    storedToken = '';
    sessionStorage.removeItem(STORAGE_KEY);
    setMode('proxy');
  },
  requireToken: () => setMode(storedToken ? 'token' : 'login'),
  probeFailed: () => setMode('error'),
  proxyUnsupported: () => setMode('unsupported'),
  login: (token: string) => {
    storedToken = token.trim();
    sessionStorage.setItem(STORAGE_KEY, storedToken);
    setMode('token');
  },
  logout: () => {
    storedToken = '';
    sessionStorage.removeItem(STORAGE_KEY);
    setMode('login');
  },
  revoke: () => {
    if (state.mode === 'proxy') {
      setMode('revoked');
    } else if (state.mode === 'token') {
      storedToken = '';
      sessionStorage.removeItem(STORAGE_KEY);
      setMode('login');
    }
  },
  subscribe: (listener: () => void) => {
    listeners.add(listener);
    return () => { listeners.delete(listener); };
  },
  getSnapshot: () => state,
};

export function useAuthState(): AuthState {
  return useSyncExternalStore(auth.subscribe, auth.getSnapshot, () => serverState);
}
