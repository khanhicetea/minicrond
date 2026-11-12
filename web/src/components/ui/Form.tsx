import { useState, type ReactNode } from 'react';
import { Icon, type IconName } from '../Icon';

/**
 * Numbered card that groups related fields. Collapsible sections show a
 * one-line `summary` of their current values while closed.
 */
export function FormSection({
  id,
  step,
  title,
  description,
  summary,
  collapsible = false,
  defaultOpen = true,
  invalid = false,
  children,
}: {
  id: string;
  step?: number;
  title: string;
  description?: ReactNode;
  summary?: ReactNode;
  collapsible?: boolean;
  defaultOpen?: boolean;
  /** Forces the section open and marks it when it holds a validation error. */
  invalid?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  const expanded = !collapsible || open || invalid;
  const heading = (
    <>
      {step !== undefined && (
        <span className={`section-step ${invalid ? 'invalid' : ''}`} aria-hidden="true">
          {step}
        </span>
      )}
      <span className="min-w-0 flex-1">
        <span className="section-title">{title}</span>
        {description && <span className="section-desc">{description}</span>}
      </span>
      {collapsible && !expanded && summary && <span className="section-summary">{summary}</span>}
      {collapsible && <Icon name="chevron-down" size={16} className={`section-chevron ${expanded ? 'open' : ''}`} />}
    </>
  );
  return (
    <section id={id} className="form-section" aria-labelledby={`${id}-title`}>
      <h2 id={`${id}-title`} className="m-0">
        {collapsible ? (
          <button type="button" className="form-section-head" aria-expanded={expanded} onClick={() => setOpen(value => !value)}>
            {heading}
          </button>
        ) : (
          <div className="form-section-head">{heading}</div>
        )}
      </h2>
      {expanded && <div className="form-section-body">{children}</div>}
    </section>
  );
}

/** Label, control, help text and inline error for one field. */
export function Field({
  id,
  label,
  help,
  error,
  optional = false,
  className = '',
  children,
}: {
  id: string;
  label: ReactNode;
  help?: ReactNode;
  error?: string;
  optional?: boolean;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={`field ${error ? 'has-error' : ''} ${className}`}>
      <label htmlFor={id} className="field-label">
        {label}
        {optional && <span className="field-optional">optional</span>}
      </label>
      {children}
      {error ? (
        <p id={`${id}-error`} className="field-error" role="alert">
          <Icon name="alert-circle" size={12} className="shrink-0" />
          {error}
        </p>
      ) : (
        help && <p className="field-help">{help}</p>
      )}
    </div>
  );
}

/** Input with a trailing unit ("sec", "MiB"). */
export function UnitInput({ unit, className = '', ...rest }: React.InputHTMLAttributes<HTMLInputElement> & { unit: string }) {
  return (
    <div className={`input-affix ${className}`}>
      <input className="mc-input" {...rest} />
      <span className="input-unit" aria-hidden="true">
        {unit}
      </span>
    </div>
  );
}

export interface Choice<T extends string> {
  value: T;
  label: string;
  description?: string;
  icon?: IconName;
}

/** Radio group rendered as selectable cards, for decisions that need a sentence of context. */
export function ChoiceCards<T extends string>({
  name,
  legend,
  value,
  options,
  onChange,
  disabled = false,
  columns = 2,
}: {
  name: string;
  legend: string;
  value: T;
  options: Choice<T>[];
  onChange: (value: T) => void;
  disabled?: boolean;
  columns?: 2 | 3;
}) {
  return (
    <fieldset className="m-0 min-w-0 border-0 p-0">
      <legend className="field-label">{legend}</legend>
      <div className={`choice-cards ${columns === 3 ? 'cols-3' : ''}`}>
        {options.map(option => (
          <label key={option.value} className={`choice-card ${value === option.value ? 'selected' : ''} ${disabled ? 'disabled' : ''}`}>
            <input type="radio" name={name} value={option.value} checked={value === option.value} disabled={disabled} onChange={() => onChange(option.value)} className="sr-only" />
            {option.icon && (
              <span className="choice-icon">
                <Icon name={option.icon} size={16} />
              </span>
            )}
            <span className="min-w-0">
              <span className="choice-label">{option.label}</span>
              {option.description && <span className="choice-desc">{option.description}</span>}
            </span>
            <span className="choice-radio" aria-hidden="true" />
          </label>
        ))}
      </div>
    </fieldset>
  );
}

/** Inline switch with a label and optional description, as one clickable row. */
export function SwitchRow({ id, label, description, checked, onChange, disabled = false }: { id: string; label: string; description?: ReactNode; checked: boolean; onChange: (checked: boolean) => void; disabled?: boolean }) {
  return (
    <label htmlFor={id} className="switch-row">
      <span className="min-w-0 flex-1">
        <span className="block text-sm font-medium">{label}</span>
        {description && <span className="field-help !mt-0.5 block">{description}</span>}
      </span>
      <span className="switch">
        <input id={id} type="checkbox" role="switch" checked={checked} disabled={disabled} onChange={event => onChange(event.target.checked)} />
        <span className="track" />
      </span>
    </label>
  );
}
