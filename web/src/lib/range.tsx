import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';

export type RangeKey = '15m' | '1h' | '24h' | '7d' | '30d';

export const RANGE_MS: Record<RangeKey, number> = {
  '15m': 15 * 60_000,
  '1h': 60 * 60_000,
  '24h': 24 * 60 * 60_000,
  '7d': 7 * 24 * 60 * 60_000,
  '30d': 30 * 24 * 60 * 60_000,
};

export const RANGE_LABEL: Record<RangeKey, string> = {
  '15m': 'Last 15 minutes',
  '1h': 'Last hour',
  '24h': 'Last 24 hours',
  '7d': 'Last 7 days',
  '30d': 'Last 30 days',
};

interface RangeContextValue {
  range: RangeKey;
  setRange: (range: RangeKey) => void;
  ms: number;
}

const RangeContext = createContext<RangeContextValue>({ range: '15m', setRange: () => {}, ms: RANGE_MS['15m'] });

/** Global time range selected in the header; scopes overview + metrics. */
export function RangeProvider({ children }: { children: ReactNode }) {
  const [range, setRange] = useState<RangeKey>(() => {
    const stored = localStorage.getItem('minicron_range');
    return stored === '15m' || stored === '1h' || stored === '24h' || stored === '7d' || stored === '30d' ? stored : '15m';
  });
  useEffect(() => {
    localStorage.setItem('minicron_range', range);
  }, [range]);
  const value = { range, setRange, ms: RANGE_MS[range] };
  return <RangeContext.Provider value={value}>{children}</RangeContext.Provider>;
}

export function useRange(): RangeContextValue {
  return useContext(RangeContext);
}
