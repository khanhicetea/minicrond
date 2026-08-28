import { Icon, type IconName } from './Icon';

interface StatusMeta {
  label: string;
  icon: IconName;
  cls: string;
  spin?: boolean;
}

const STATUS_META: Record<string, StatusMeta> = {
  succeeded: { label: 'Success', icon: 'circle-check', cls: 'chip-success' },
  failed: { label: 'Failed', icon: 'circle-x', cls: 'chip-error' },
  timeout: { label: 'Timeout', icon: 'alert-circle', cls: 'chip-error' },
  interrupted: { label: 'Interrupted', icon: 'alert-triangle', cls: 'chip-warn' },
  stopped: { label: 'Stopped', icon: 'square', cls: 'chip-warn' },
  skipped: { label: 'Skipped', icon: 'pause', cls: 'chip-warn' },
  missed: { label: 'Missed', icon: 'alert-triangle', cls: 'chip-warn' },
  running: { label: 'Running', icon: 'loader', cls: 'chip-info', spin: true },
  pending: { label: 'Pending', icon: 'clock', cls: 'chip-info' },
};

/** Tinted status pill with icon, matching the reference design. */
export default function StatusBadge({ status, size = 'sm' }: { status: string; size?: 'sm' | 'md' }) {
  const meta = STATUS_META[status] ?? { label: status, icon: 'info' as IconName, cls: 'chip-neutral' };
  return (
    <span className={`chip ${meta.cls} ${size === 'md' ? 'text-xs px-2.5 py-1' : ''}`}>
      <Icon name={meta.icon} size={size === 'md' ? 13 : 12} className={meta.spin ? 'spin' : ''} strokeWidth={2.2} />
      {meta.label}
    </span>
  );
}

/** Small status icon without the pill (used in run lists). */
export function StatusIcon({ status, size = 14 }: { status: string; size?: number }) {
  const meta = STATUS_META[status] ?? { icon: 'info' as IconName, cls: '' };
  const color =
    status === 'succeeded'
      ? 'text-green-400'
      : status === 'failed' || status === 'timeout'
        ? 'text-red-400'
        : status === 'running' || status === 'pending'
          ? 'text-blue-400'
          : 'text-amber-400';
  return <Icon name={meta.icon} size={size} className={`${color} ${meta.spin ? 'spin' : ''} shrink-0`} />;
}
