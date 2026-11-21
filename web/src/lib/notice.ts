import { useSyncExternalStore } from 'react';

export interface Notice {
  id: number;
  tone: 'ok' | 'info';
  title: string;
  detail?: string;
}

/** How long a notice stays up before it dismisses itself. */
const NOTICE_MS = 5_000;

let current: Notice | null = null;
let timer: ReturnType<typeof setTimeout> | undefined;
let nextId = 1;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of listeners) listener();
}

/**
 * Show one app-wide confirmation banner (e.g. "Job saved"). It survives route
 * changes, replaces any notice already shown, and clears itself after a few
 * seconds; only one timer exists, and only while a notice is visible.
 */
export function showNotice(notice: Omit<Notice, 'id'>) {
  current = { ...notice, id: nextId++ };
  clearTimeout(timer);
  timer = setTimeout(dismissNotice, NOTICE_MS);
  emit();
}

export function dismissNotice() {
  clearTimeout(timer);
  timer = undefined;
  if (!current) return;
  current = null;
  emit();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useNotice(): Notice | null {
  return useSyncExternalStore(subscribe, () => current);
}
