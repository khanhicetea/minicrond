import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { Router } from 'wouter';
import { api, ApiError } from './api';
import { auth } from './auth';
import App from './App';
import './index.css';

// Apply the stored theme before first paint to avoid a wrong-theme flash.
// The custom dark theme mirrors the reference design and is the default.
const storedTheme = localStorage.getItem('minicron_theme');
document.documentElement.dataset.theme =
  storedTheme === 'light' || storedTheme === 'dark'
    ? storedTheme
    : window.matchMedia('(prefers-color-scheme: light)').matches
      ? 'light'
      : 'dark';

// Keep cached API data scoped to the active browser session.
const queryClient = new QueryClient({
  queryCache: new QueryCache({
    onError: error => {
      if (error instanceof ApiError && error.status === 401) auth.revoke();
    },
  }),
  mutationCache: new MutationCache({
    onError: error => {
      if (error instanceof ApiError && error.status === 401) auth.revoke();
    },
  }),
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => (error instanceof ApiError && error.status < 500 ? false : failureCount < 2),
      staleTime: 5_000,
      refetchOnWindowFocus: true,
    },
    mutations: { retry: false },
  },
});

auth.subscribe(() => queryClient.clear());

void api.probeTransport().then(
  daemon => {
    if (daemon.tcp_enabled !== false) auth.proxyUnsupported();
    else auth.useProxy();
  },
  error => {
    if (error instanceof ApiError && error.status === 401) auth.requireToken();
    else auth.probeFailed();
  },
);

createRoot(document.getElementById('app-root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      {/* Path-based routing: the Go daemon serves the SPA shell for every UI
          route, so the browser history API works (shareable /jobs, /runs/x). */}
      <Router>
        <App />
      </Router>
    </QueryClientProvider>
  </StrictMode>,
);
