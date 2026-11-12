import type { ReactNode } from 'react';
import { Icon, type IconName } from '../Icon';
import { TONE_CHIP, TONE_DOT, type Tone } from './status';

/** Tinted pill for a status or short label. */
export function Badge({ tone = 'neutral', icon, spin = false, className = '', children }: { tone?: Tone; icon?: IconName; spin?: boolean; className?: string; children: ReactNode }) {
  return (
    <span className={`chip ${TONE_CHIP[tone]} ${className}`}>
      {icon && <Icon name={icon} size={12} strokeWidth={2.4} className={spin ? 'spin' : ''} />}
      {children}
    </span>
  );
}

/** Round status dot. */
export function Dot({ tone = 'neutral', pulse = false, className = '' }: { tone?: Tone; pulse?: boolean; className?: string }) {
  return <span className={`dot ${TONE_DOT[tone]} ${pulse ? 'dot-pulse' : ''} ${className}`} aria-hidden="true" />;
}

/** Dot plus text, both in the status tone ("● Failed"). */
export function StatusLabel({ tone, children, className = '' }: { tone: Tone; children: ReactNode; className?: string }) {
  return (
    <span className={`status-label tone-${tone} ${className}`}>
      <Dot tone={tone} />
      {children}
    </span>
  );
}
