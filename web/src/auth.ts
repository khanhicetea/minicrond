import { useSyncExternalStore } from 'react';

/**
 * Bearer token store. The token lives in this tab's sessionStorage only, so a
 * browser restart (or a new tab) requires signing in again. Components observe
 * it through useAuthToken().
 */
const STORAGE_KEY = 'minicron_token';

let current: string = sessionStorage.getItem(STORAGE_KEY) ?? '';
const listeners = new Set<() => void>();

function emit() {
  for (const listener of listeners) listener();
}

function setToken(token: string) {
  current = token.trim();
  if (current) sessionStorage.setItem(STORAGE_KEY, current);
  else sessionStorage.removeItem(STORAGE_KEY);
  emit();
}

export const auth = {
  get token() {
    return current;
  },
  login: (token: string) => setToken(token),
  logout: () => setToken(''),
  subscribe: (listener: () => void) => {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  },
  getSnapshot: () => current,
};

export function useAuthToken(): string {
  return useSyncExternalStore(auth.subscribe, auth.getSnapshot, () => '');
}
