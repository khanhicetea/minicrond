import { useMemo } from 'react';
import { barY, defineChart, lineY } from '@tanstack/charts';
import { scaleLinear } from '@tanstack/charts/scales/linear';
import { tooltip } from '@tanstack/charts/tooltip';
import { Chart } from '@tanstack/charts/react';

export interface Series {
  name: string;
  color: string;
  points: (number | null)[];
}

const GRID = 'color-mix(in srgb, var(--color-base-content) 13%, transparent)';
const TEXT = 'color-mix(in srgb, var(--color-base-content) 55%, transparent)';
const BG = 'transparent';

function niceScale(max: number): { ticks: number[]; top: number } {
  if (max <= 0) return { ticks: [0, 1], top: 1 };
  const exp = Math.floor(Math.log10(max));
  const base = 10 ** exp;
  const scaled = max / base;
  const top = (scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 5 ? 5 : 10) * base;
  const ticks = [0, top / 4, top / 2, (3 * top) / 4, top];
  return { ticks, top };
}

function labelFor(labels: string[], count: number, value: number) {
  if (labels.length === 0 || count <= 1) return String(value);
  const labelIndex = Math.round((value / (count - 1)) * (labels.length - 1));
  return labels[Math.max(0, Math.min(labelIndex, labels.length - 1))] ?? String(value);
}

function theme(palette: readonly string[]) {
  return {
    foreground: TEXT,
    muted: TEXT,
    grid: GRID,
    background: BG,
    palette,
  };
}

interface LineRow {
  index: number;
  series: string;
  value: number | null;
  color: string;
  label: string;
}

export function LineChart({
  series,
  labels,
  height = 220,
  formatY = v => String(v),
}: {
  series: Series[];
  labels: string[];
  height?: number;
  log?: boolean;
  formatY?: (value: number) => string;
}) {
  const count = Math.max(...series.map(s => s.points.length), 1);
  const rows = useMemo(
    () =>
      series.flatMap(item =>
        item.points.map((value, index): LineRow => ({
          index,
          series: item.name,
          value,
          color: item.color,
          label: labelFor(labels, count, index),
        })),
      ),
    [series, labels],
  );

  const definition = useMemo(() => {
    const { top } = niceScale(Math.max(...rows.map(row => row.value ?? 0), 0));
    return defineChart({
      marks: [
        lineY(rows, {
          x: 'index',
          y: 'value',
          z: 'series',
          stroke: row => row.color,
          strokeWidth: 1.75,
        }),
      ],
      scales: {
        x: {
          scale: scaleLinear().domain([0, Math.max(count - 1, 1)]),
          axis: {
            ticks: { values: [0, Math.floor(count / 3), Math.floor((2 * count) / 3), count - 1], format: value => labelFor(labels, count, value) },
            tickLabels: { thin: false },
          },
        },
        y: {
          scale: scaleLinear().domain([0, top]),
          grid: true,
          axis: { ticks: { count: 5, format: formatY } },
        },
      },
      clip: true,
      margin: { top: 12, right: 12, bottom: 24, left: 44 },
      theme: theme(series.map(item => item.color)),
      focus: 'group-x',
      tooltip: {
        use: tooltip,
        formatGroup(points) {
          const heading = points[0]?.datum.label ?? '';
          return [heading, ...points.map(point => `${point.datum.series}: ${formatY(point.yValue)}`)].join('\n');
        },
        format(point) {
          return `${point.datum.label}\n${point.datum.series}: ${formatY(point.yValue)}`;
        },
      },
    });
  }, [rows, count, labels, formatY, series]);

  return <Chart definition={definition} height={height} ariaLabel="Line chart" />;
}

export function StepChart({
  series,
  labels,
  height = 220,
  formatY = v => String(v),
}: {
  series: Series[];
  labels: string[];
  height?: number;
  formatY?: (value: number) => string;
}) {
  return <LineChart series={series} labels={labels} height={height} formatY={formatY} />;
}

interface BarRow {
  index: number;
  status: 'Success' | 'Failure';
  value: number;
  label: string;
}

export function StackedBars({
  buckets,
  labels,
  height = 220,
  formatY = v => String(v),
}: {
  buckets: { success: number; failure: number }[];
  labels: string[];
  height?: number;
  formatY?: (value: number) => string;
}) {
  const count = Math.max(buckets.length, 1);
  const rows = useMemo(
    () =>
      buckets.flatMap((bucket, index): BarRow[] => [
        { index, status: 'Success', value: bucket.success, label: labelFor(labels, count, index) },
        { index, status: 'Failure', value: bucket.failure, label: labelFor(labels, count, index) },
      ]),
    [buckets, labels, count],
  );

  const definition = useMemo(() => {
    const { top } = niceScale(Math.max(...buckets.map(bucket => bucket.success + bucket.failure), 0));
    return defineChart({
      marks: [
        barY(rows, {
          x: 'index',
          y: 'value',
          color: 'status',
          fill: row => (row.status === 'Success' ? '#34d399' : '#f87171'),
          fillOpacity: 0.9,
          radius: 1,
        }),
      ],
      scales: {
        x: {
          scale: scaleLinear().domain([-0.5, Math.max(count - 0.5, 0.5)]),
          axis: {
            ticks: { values: [0, Math.floor(count / 3), Math.floor((2 * count) / 3), count - 1], format: value => labelFor(labels, count, value) },
            tickLabels: { thin: false },
          },
        },
        y: {
          scale: scaleLinear().domain([0, top]),
          grid: true,
          axis: { ticks: { count: 5, format: formatY } },
        },
      },
      color: { domain: ['Success', 'Failure'], range: ['#34d399', '#f87171'] },
      clip: true,
      margin: { top: 12, right: 12, bottom: 24, left: 30 },
      theme: theme(['#34d399', '#f87171']),
      focus: 'group-x',
      tooltip: {
        use: tooltip,
        formatGroup(points) {
          const heading = points[0]?.datum.label ?? '';
          return [heading, ...points.map(point => `${point.datum.status}: ${formatY(point.datum.value)}`)].join('\n');
        },
        format(point) {
          return `${point.datum.label}\n${point.datum.status}: ${formatY(point.datum.value)}`;
        },
      },
    });
  }, [rows, buckets, count, labels, formatY]);

  return <Chart definition={definition} height={height} ariaLabel="Stacked bar chart" />;
}

/** Legend chip used in panel headers. */
export function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 text-xs muted">
      <span className="inline-block h-[3px] w-3.5 rounded-full" style={{ background: color }} />
      {label}
    </span>
  );
}
