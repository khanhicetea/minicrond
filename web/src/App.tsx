import { Route, Switch } from 'wouter';
import { useAuthToken } from './auth';
import Layout from './components/Layout';
import Dashboard from './pages/Dashboard';
import JobCreate from './pages/JobCreate';
import JobDetail from './pages/JobDetail';
import Login from './pages/Login';
import RunDetail from './pages/RunDetail';

function NotFound() {
  return (
    <div className="rounded-box border border-base-300 bg-base-100 p-10 text-center">
      <p className="text-4xl">🧭</p>
      <h1 className="mt-2 text-xl font-bold">Page not found</h1>
      <p className="mt-1 text-sm text-base-content/60">The address does not match any minicron page.</p>
    </div>
  );
}

export default function App() {
  const token = useAuthToken();
  if (!token) return <Login />;
  return (
    <Layout>
      <Switch>
        <Route path="/" component={Dashboard} />
        <Route path="/jobs/new" component={JobCreate} />
        <Route path="/jobs/:name" component={JobDetail} />
        <Route path="/runs/:id" component={RunDetail} />
        <Route component={NotFound} />
      </Switch>
    </Layout>
  );
}
