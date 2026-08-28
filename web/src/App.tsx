import { Route, Switch } from 'wouter';
import { useAuthToken } from './auth';
import { RangeProvider } from './lib/range';
import Layout from './components/Layout';
import Dashboard from './pages/Dashboard';
import JobEditor from './pages/JobEditor';
import Jobs from './pages/Jobs';
import Login from './pages/Login';
import Metrics from './pages/Metrics';
import RunDetail from './pages/RunDetail';
import Runs from './pages/Runs';
import Settings from './pages/Settings';
import { Icon } from './components/Icon';

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
  const token = useAuthToken();
  if (!token) return <Login />;
  return (
    <RangeProvider>
      <Layout>
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
      </Layout>
    </RangeProvider>
  );
}
