export interface SegmentedOption<T extends string> {
  value: T;
  label: string;
  count?: number;
  /** Hide the option entirely (e.g. "Disabled" when there are none). */
  hidden?: boolean;
}

/** Single-choice filter group; each option is a toggle button with aria-pressed. */
export function Segmented<T extends string>({
  options,
  value,
  onChange,
  label,
  size = 'md',
  className = '',
}: {
  options: SegmentedOption<T>[];
  value: T;
  onChange: (value: T) => void;
  label: string;
  size?: 'sm' | 'md';
  className?: string;
}) {
  return (
    <div role="group" aria-label={label} className={`segmented ${size === 'sm' ? 'segmented-sm' : ''} ${className}`}>
      {options
        .filter(option => !option.hidden)
        .map(option => (
          <button key={option.value} type="button" aria-pressed={value === option.value} onClick={() => onChange(option.value)}>
            {option.label}
            {option.count !== undefined && <span className="count">{option.count}</span>}
          </button>
        ))}
    </div>
  );
}
