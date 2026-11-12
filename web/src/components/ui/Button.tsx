import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import { Icon, type IconName } from '../Icon';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

const VARIANT: Record<ButtonVariant, string> = {
  primary: 'btn-primary-x',
  secondary: 'btn-sub',
  ghost: 'btn-ghost-x',
  danger: 'btn-danger-x',
};

/**
 * Class string for a button-styled element. Use it on wouter <Link>s so
 * navigation stays a real link while looking like a Button.
 */
export function buttonClass(variant: ButtonVariant = 'secondary', size: ButtonSize = 'md', extra = ''): string {
  return [VARIANT[variant], size === 'sm' ? 'btn-sm' : size === 'lg' ? 'btn-lg' : '', extra].filter(Boolean).join(' ');
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  icon?: IconName;
  /** Shows a spinner in place of the icon and disables the button. */
  busy?: boolean;
  children?: ReactNode;
}

export function Button({ variant = 'secondary', size = 'md', icon, busy = false, className = '', type = 'button', disabled, children, ...rest }: ButtonProps) {
  return (
    <button type={type} className={buttonClass(variant, size, className)} disabled={disabled || busy} {...rest}>
      {busy ? <Icon name="loader" size={14} className="spin" /> : icon ? <Icon name={icon} size={size === 'sm' ? 13 : 14} /> : null}
      {children}
    </button>
  );
}

interface IconButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'children'> {
  icon: IconName;
  /** Accessible name; also used as the tooltip unless `title` is given. */
  label: string;
  size?: 'sm' | 'md';
  outlined?: boolean;
  iconSize?: number;
  iconClassName?: string;
}

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(function IconButton(
  { icon, label, size = 'md', outlined = false, iconSize, iconClassName, className = '', type = 'button', title, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      aria-label={label}
      title={title ?? label}
      className={['btn-icon', size === 'sm' ? 'sm' : '', outlined ? 'outlined' : '', className].filter(Boolean).join(' ')}
      {...rest}
    >
      <Icon name={icon} size={iconSize ?? (size === 'sm' ? 14 : 16)} className={iconClassName} />
    </button>
  );
});
