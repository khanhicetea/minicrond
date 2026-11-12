import type { ReactNode } from 'react';
import { Icon, type IconName } from '../Icon';
import type { Tone } from './status';

/** Page title block: title, optional badge and subtitle, right-side actions. */
export function PageHeader({ title, subtitle, badge, actions }: { title: ReactNode; subtitle?: ReactNode; badge?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="page-header">
      <div className="min-w-0 flex-[1_1_20rem]">
        <div className="flex flex-wrap items-center gap-2.5">
          <h1 className="page-title">{title}</h1>
          {badge}
        </div>
        {subtitle && <p className="page-subtitle">{subtitle}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  );
}

/** Bordered surface. `as` keeps landmarks semantic (section, aside). */
export function Card({ as: Tag = 'div', className = '', children, ...rest }: { as?: 'div' | 'section' | 'aside'; className?: string; children: ReactNode; 'aria-label'?: string; 'aria-labelledby'?: string }) {
  return (
    <Tag className={`panel ${className}`} {...rest}>
      {children}
    </Tag>
  );
}

export function CardHeader({ title, id, meta, actions }: { title: ReactNode; id?: string; meta?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="card-header">
      <div className="flex min-w-0 items-center gap-2">
        <h2 id={id} className="panel-title">
          {title}
        </h2>
        {meta && <span className="text-xs faint">{meta}</span>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

const CALLOUT_ICON: Record<Exclude<Tone, 'ok' | 'neutral'>, IconName> = {
  danger: 'alert-triangle',
  warn: 'alert-circle',
  info: 'info',
};

/**
 * Toned banner. With `alert`, it is announced (errors); otherwise it is a
 * labelled region (the "needs attention" summary).
 */
export function Callout({
  tone = 'info',
  title,
  meta,
  actions,
  alert = false,
  children,
  className = '',
}: {
  tone?: 'danger' | 'warn' | 'info';
  title: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  alert?: boolean;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <section role={alert ? 'alert' : undefined} className={`callout callout-${tone} ${className}`}>
      <div className="callout-head">
        <Icon name={CALLOUT_ICON[tone]} size={16} className="callout-icon shrink-0" />
        <h2 className="callout-title">{title}</h2>
        {meta && <span className="text-sm muted">{meta}</span>}
        {actions && <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
      {children}
    </section>
  );
}

/** Inline error/warning line used for request failures. */
export function InlineAlert({ tone = 'danger', children, action }: { tone?: 'danger' | 'warn'; children: ReactNode; action?: ReactNode }) {
  return (
    <div role="alert" className={`callout callout-${tone} flex flex-wrap items-center gap-3 px-4 py-2.5 text-sm`}>
      <Icon name={tone === 'danger' ? 'alert-circle' : 'alert-triangle'} size={15} className="callout-icon shrink-0" />
      <span className="min-w-0 flex-1">{children}</span>
      {action}
    </div>
  );
}

export function EmptyState({ icon = 'inbox', title, children }: { icon?: IconName; title: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-14 text-center">
      <Icon name={icon} size={24} className="faint" />
      <p className="text-sm font-medium">{title}</p>
      {children && <div className="text-sm muted">{children}</div>}
    </div>
  );
}

/** Row of headline numbers (run detail). */
export function Facts({ items, label }: { label: string; items: { label: string; value: ReactNode; tone?: 'ok' | 'danger' | 'info'; title?: string }[] }) {
  return (
    <dl className="facts" aria-label={label}>
      {items.map(item => (
        <div key={item.label} className={`fact ${item.tone ? `fact-${item.tone}` : ''}`} title={item.title}>
          <dt>{item.label}</dt>
          <dd>{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}

/** Label/value pairs in a side card. */
export function KeyValues({ items }: { items: { label: string; value: ReactNode; mono?: boolean }[] }) {
  return (
    <dl className="kv">
      {items.map(item => (
        <div key={item.label} className="contents">
          <dt>{item.label}</dt>
          <dd className={item.mono ? 'mono' : ''}>{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}
