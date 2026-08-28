import { useState } from 'react';
import { useLocation, useParams } from 'wouter';
import JobForm, { emptyDefinition } from '../components/JobForm';
import { errorText } from '../api';
import { useSaveJob } from '../queries';
import type { Definition } from '../types';

/** Create a new DB-authority definition. */
export default function JobCreate() {
  const params = useParams<{ kind?: string }>();
  const kind = params.kind === 'worker' ? 'worker' : 'job';
  const [, navigate] = useLocation();
  const save = useSaveJob();
  const [error, setError] = useState('');

  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-bold">New definition</h1>
      <p className="max-w-2xl text-sm text-base-content/70">
        Definitions created here are stored with <code className="font-mono">db</code> authority and applied
        immediately. File-managed definitions must be edited in their TOML source instead.
      </p>
      <JobForm
        creating
        initial={emptyDefinition(kind)}
        readOnly={false}
        submitting={save.isPending}
        error={error || (save.error ? errorText(save.error) : '')}
        onSubmit={(definition: Definition) => {
          setError('');
          save.mutate(
            { definition },
            {
              onSuccess: saved => navigate(`/jobs/${saved.name}`),
              onError: err => setError(errorText(err)),
            },
          );
        }}
      />
    </div>
  );
}
