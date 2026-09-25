import { Component, lazy, Suspense, type ReactNode } from 'react';
import { Route, Switch, useLocation } from 'wouter';
import { useAuthState } from './auth';
import { RangeProvider } from './lib/range';
import Layout from './components/Layout';
import Login from './pages/Login';
import { Icon } from './components/Icon';

const Dashboard = lazy(() => import('./pages/Dashboard'));
const JobEditor = lazy(() => import('./pages/JobEditor'));
const Jobs = lazy(() => import('./pages/Jobs'));
const Metrics = lazy(() => import('./pages/Metrics'));
const RunDetail = lazy(() => import('./pages/RunDetail'));
const Runs = lazy(() => import('./pages/Runs'));
const Settings = lazy(() => import('./pages/Settings'));

class PageErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false };

  static getDerivedStateFromError() {
    return { failed: true };
  }

  render() {
    if (!this.state.failed) return this.props.children;
    return (
      <div role="alert" className="panel mx-auto max-w-md px-8 py-10 text-center">
        <p className="text-sm">This page could not be loaded. The app may have been updated.</p>
        <button type="button" className="btn-primary-x mt-4" onClick={() => window.location.reload()}>Reload app</button>
      </div>
    );
  }
}

function NotFound() {
  return (
    <div className="panel mx-auto max-w-md px-8 py-12 text-center">
      <Icon name="search" size={28} className="mx-auto faint" />
      <h1 className="mt-3 text-xl font-bold">Page not found</h1>
      <p className="mt-1 text-sm muted">The address does not match any minicron page.</p>
    </div>
  );
}

export default function App() {
  const { mode } = useAuthState();
  const [location] = useLocation();
  if (mode === 'checking') return <div className="skeleton h-64" aria-label="Checking daemon access" />;
  if (mode === 'login') return <Login />;
  if (mode === 'revoked' || mode === 'error' || mode === 'unsupported') {
    return (
      <div role="alert" className="panel mx-auto mt-20 max-w-md px-8 py-10">
        <h1 className="text-xl font-semibold">{mode === 'revoked' ? 'Access ended' : mode === 'unsupported' ? 'Proxy setup needs Unix-only mode' : 'Cannot check daemon access'}</h1>
        <p className="mt-2 text-sm muted">{mode === 'revoked' ? 'Your proxy session no longer has access to this daemon.' : mode === 'unsupported' ? 'Set server.tcp_enabled = false before serving this UI through a proxy.' : 'The daemon or its proxy is unavailable.'}</p>
        <button type="button" className="btn-primary-x mt-5" onClick={() => window.location.reload()}>Retry</button>
      </div>
    );
  }
  return (
    <RangeProvider>
      <Layout>
        <PageErrorBoundary key={location}>
          <Suspense fallback={<div className="skeleton h-64 w-full" aria-label="Loading page" />}>
            <Switch>
              <Route path="/" component={Dashboard} />
              <Route path="/jobs" component={Jobs} />
              <Route path="/jobs/new" component={JobEditor} />
              <Route path="/jobs/:name/*?" component={JobEditor} />
              <Route path="/runs" component={Runs} />
              <Route path="/runs/:id" component={RunDetail} />
              <Route path="/metrics" component={Metrics} />
              <Route path="/settings" component={Settings} />
              <Route component={NotFound} />
            </Switch>
          </Suspense>
        </PageErrorBoundary>
      </Layout>
    </RangeProvider>
  );
}
