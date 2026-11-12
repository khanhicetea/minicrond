import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import { createPortal } from 'react-dom';
import { Icon, type IconName } from '../Icon';
import { IconButton } from './Button';

export interface ActionItem {
  label: string;
  icon?: IconName;
  onSelect: () => void;
  disabled?: boolean;
  /** Emphasized first action (e.g. "Run now"). */
  primary?: boolean;
  danger?: boolean;
  /** Renders an on/off glyph and exposes the item as a menuitemcheckbox. */
  checked?: boolean;
  /** Draw a divider above this item. */
  separatorBefore?: boolean;
  hidden?: boolean;
}

/**
 * "⋯" button that opens a menu of row actions. The menu is portalled and
 * fixed-positioned so it is never clipped by scrolling tables, flips above the
 * button near the viewport bottom, and supports arrow-key navigation.
 */
export function ActionMenu({ label, items, icon = 'more-horizontal' }: { label: string; items: ActionItem[]; icon?: IconName }) {
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const buttonRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const visible = items.filter(item => !item.hidden);

  useLayoutEffect(() => {
    if (!open || !buttonRef.current || !menuRef.current) return;
    const anchor = buttonRef.current.getBoundingClientRect();
    const menu = menuRef.current.getBoundingClientRect();
    const margin = 8;
    const below = anchor.bottom + 4;
    setPosition({
      left: Math.max(margin, Math.min(anchor.right - menu.width, window.innerWidth - menu.width - margin)),
      top: Math.max(margin, Math.min(below + menu.height <= window.innerHeight - margin ? below : anchor.top - menu.height - 4, window.innerHeight - menu.height - margin)),
    });
    menuRef.current.querySelector<HTMLButtonElement>('button:not(:disabled)')?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onPointer = (event: MouseEvent) => {
      const target = event.target as Node;
      if (!menuRef.current?.contains(target) && !buttonRef.current?.contains(target)) setOpen(false);
    };
    const onScroll = (event: Event) => {
      if (!(event.target instanceof Node) || !menuRef.current?.contains(event.target)) setOpen(false);
    };
    const close = () => setOpen(false);
    document.addEventListener('mousedown', onPointer);
    document.addEventListener('scroll', onScroll, true);
    window.addEventListener('resize', close);
    return () => {
      document.removeEventListener('mousedown', onPointer);
      document.removeEventListener('scroll', onScroll, true);
      window.removeEventListener('resize', close);
    };
  }, [open]);

  const onMenuKey = (event: ReactKeyboardEvent<HTMLDivElement>) => {
    const buttons = [...(menuRef.current?.querySelectorAll<HTMLButtonElement>('button:not(:disabled)') ?? [])];
    const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
    if (event.key === 'Escape') {
      event.preventDefault();
      setOpen(false);
      buttonRef.current?.focus();
    } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const step = event.key === 'ArrowDown' ? 1 : -1;
      buttons[(index + step + buttons.length) % buttons.length]?.focus();
    } else if (event.key === 'Home' || event.key === 'End') {
      event.preventDefault();
      buttons[event.key === 'Home' ? 0 : buttons.length - 1]?.focus();
    } else if (event.key === 'Tab') {
      setOpen(false);
    }
  };

  const select = (item: ActionItem) => {
    setOpen(false);
    buttonRef.current?.focus();
    item.onSelect();
  };

  return (
    <>
      <IconButton ref={buttonRef} icon={icon} label={label} aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(value => !value)} />
      {open &&
        createPortal(
          <div
            ref={menuRef}
            role="menu"
            aria-label={label}
            className="menu-pop"
            onKeyDown={onMenuKey}
            style={{ position: 'fixed', ...position, maxWidth: 'calc(100vw - 16px)', maxHeight: 'calc(100dvh - 16px)', overflowY: 'auto' }}
          >
            {visible.map(item => (
              <div key={item.label} className="contents">
                {item.separatorBefore && <div role="separator" className="menu-sep" />}
                <button
                  type="button"
                  role={item.checked === undefined ? 'menuitem' : 'menuitemcheckbox'}
                  aria-checked={item.checked}
                  disabled={item.disabled}
                  className={`menu-item ${item.primary ? 'primary' : ''} ${item.danger ? 'danger' : ''}`}
                  onClick={() => select(item)}
                >
                  {item.checked !== undefined ? (
                    <span className={`switch-glyph ${item.checked ? 'on' : ''}`} aria-hidden="true" />
                  ) : item.icon ? (
                    <Icon name={item.icon} size={14} />
                  ) : (
                    <span className="w-3.5" aria-hidden="true" />
                  )}
                  <span className="flex-1">{item.label}</span>
                </button>
              </div>
            ))}
          </div>,
          document.body,
        )}
    </>
  );
}
