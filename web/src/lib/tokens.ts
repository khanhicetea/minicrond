import { useEffect, useState } from 'react';

/**
 * Resolved design-token colors for canvas-rendered charts, which cannot read
 * CSS custom properties themselves. Values come from styles/tokens.css and are
 * re-read whenever the theme attribute on <html> changes.
 */
export interface ChartColors {
  ok: string;
  danger: string;
  warn: string;
  info: string;
  text: string;
  grid: string;
}

const FALLBACK: ChartColors = { ok: '#3fb97f', danger: '#ff8a65', warn: '#f2b33d', info: '#5b9bff', text: '#9aa4b2', grid: '#232a35' };

export function readChartColors(): ChartColors {
  if (typeof document === 'undefined') return FALLBACK;
  const style = getComputedStyle(document.documentElement);
  const read = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  return {
    ok: read('--mc-ok', FALLBACK.ok),
    danger: read('--mc-danger', FALLBACK.danger),
    warn: read('--mc-warn', FALLBACK.warn),
    info: read('--mc-info', FALLBACK.info),
    text: read('--mc-fg-muted', FALLBACK.text),
    grid: read('--mc-line', FALLBACK.grid),
  };
}

export function useChartColors(): ChartColors {
  const [colors, setColors] = useState(readChartColors);
  useEffect(() => {
    const observer = new MutationObserver(() => setColors(readChartColors()));
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => observer.disconnect();
  }, []);
  return colors;
}
