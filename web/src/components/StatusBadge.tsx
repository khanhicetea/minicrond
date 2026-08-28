const STATUS_CLASS: Record<string, string> = {
  succeeded: 'badge-success',
  failed: 'badge-error',
  timeout: 'badge-error',
  interrupted: 'badge-warning',
  stopped: 'badge-warning',
  skipped: 'badge-warning',
  missed: 'badge-warning',
  running: 'badge-info',
  pending: 'badge-info badge-outline',
};

export default function StatusBadge({ status }: { status: string }) {
  const pulse = status === 'running' ? 'badge-info' : '';
  return (
    <span className={`badge badge-sm whitespace-nowrap ${STATUS_CLASS[status] ?? 'badge-ghost'} ${pulse}`}>
      {status === 'running' && <span className="loading loading-spinner loading-xs" aria-hidden />}
      {status}
    </span>
  );
}
