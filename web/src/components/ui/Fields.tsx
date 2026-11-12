import { forwardRef, type InputHTMLAttributes, type ReactNode } from 'react';
import { Icon } from '../Icon';

interface SearchFieldProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'onChange' | 'size'> {
  value: string;
  onChange: (value: string) => void;
  /** Accessible name for the input. */
  label: string;
  size?: 'sm' | 'md';
  /** Trailing hint such as a keyboard shortcut. */
  hint?: ReactNode;
}

/** Icon + text input + optional hint, styled as one control. */
export const SearchField = forwardRef<HTMLInputElement, SearchFieldProps>(function SearchField(
  { value, onChange, label, size = 'md', hint, className = '', placeholder, ...rest },
  ref,
) {
  return (
    <label className={`search-field ${size === 'sm' ? 'sm' : ''} ${className}`}>
      <Icon name="search" size={size === 'sm' ? 13 : 14} className="shrink-0" />
      <span className="sr-only">{label}</span>
      <input ref={ref} type="search" value={value} onChange={event => onChange(event.target.value)} placeholder={placeholder ?? label} {...rest} />
      {hint}
    </label>
  );
});

/** Accessible on/off switch backed by a checkbox. */
export function Switch({
  checked,
  onChange,
  label,
  disabled = false,
  tone = 'accent',
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label: string;
  disabled?: boolean;
  tone?: 'accent' | 'ok';
}) {
  return (
    <label className={`switch ${tone === 'ok' ? 'green' : ''}`}>
      <input type="checkbox" role="switch" checked={checked} disabled={disabled} aria-label={label} onChange={event => onChange(event.target.checked)} />
      <span className="track" />
    </label>
  );
}
