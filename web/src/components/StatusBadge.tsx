import { Icon } from './Icon';
import { Badge } from './ui/Badge';
import { runStatusMeta } from './ui/status';

/** Tinted run-status pill ("✕ Failed"). */
export default function StatusBadge({ status, size = 'sm' }: { status: string; size?: 'sm' | 'md' }) {
  const meta = runStatusMeta(status);
  return (
    <Badge tone={meta.tone} icon={meta.icon} spin={meta.spin} className={size === 'md' ? 'text-[0.8125rem]' : ''}>
      {meta.label}
    </Badge>
  );
}

/** Status icon without the pill (used in run lists). */
export function StatusIcon({ status, size = 14 }: { status: string; size?: number }) {
  const meta = runStatusMeta(status);
  return <Icon name={meta.icon} size={size} className={`tone-${meta.tone} ${meta.spin ? 'spin' : ''} shrink-0`} />;
}
