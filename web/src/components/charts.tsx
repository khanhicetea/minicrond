/**
 * Dependency-free SVG charts for the metrics page, styled after the
 * reference design: flat panel charts with subtle grid, direct legend chips
 * in the panel header, and time labels on the x axis.
 */

export interface Series {
  name: string;
  color: string;
  points: (number | null)[];
}

const GRID = 'var(--color-base-300)';
const TEXT = 'color-mix(in srgb, var(--color-base-content) 45%, transparent)';

function niceScale(max: number): { ticks: number[]; top: number } {
  if (max <= 0) return { ticks: [0, 1], top: 1 };
  const exp = Math.floor(Math.log10(max));
  const base = 10 ** exp;
  const scaled = max / base;
  const top = (scaled <= 1 ? 1 : scaled <= 2 ? 2 : scaled <= 5 ? 5 : 10) * base;
  const ticks = [0, top / 4, top / 2, (3 * top) / 4, top];
  return { ticks, top };
}

function logTicks(min: number, max: number): { ticks: number[]; lo: number; hi: number } {
  const lo = Math.max(min, 0.01);
  const hi = Math.max(max * 1.2, lo * 10);
  const ticks: number[] = [];
  for (let t = 0.01; t <= hi * 1.5; t *= 10) {
    if (t >= lo / 3) ticks.push(t);
    for (const m of [2, 5]) if (t * m <= hi * 1.5 && t * m >= lo / 3) ticks.push(t * m);
  }
  return { ticks, lo, hi };
}

export function LineChart({
  series,
  labels,
  height = 220,
  log = false,
  formatY = v => String(v),
}: {
  series: Series[];
  labels: string[];
  height?: number;
  log?: boolean;
  formatY?: (value: number) => string;
}) {
  const W = 600;
  const H = height;
  const padL = 44;
  const padR = 12;
  const padT = 12;
  const padB = 24;
  const innerW = W - padL - padR;
  const innerH = H - padT - padB;
  const count = Math.max(...series.map(s => s.points.length), 1);

  let scale: { ticks: number[]; top?: number; lo?: number; hi?: number };
  if (log) {
    const values = series.flatMap(s => s.points.filter((p): p is number => p !== null && p > 0));
    scale = logTicks(Math.min(...values, 1), Math.max(...values, 1));
  } else {
    const max = Math.max(...series.flatMap(s => s.points.filter((p): p is number => p !== null)), 0);
    scale = niceScale(max);
  }

  const x = (i: number) => padL + (count <= 1 ? innerW / 2 : (i / (count - 1)) * innerW);
  const y = (v: number) => {
    if (log) {
      const { lo, hi } = scale as { lo: number; hi: number };
      const clamped = Math.max(v, lo);
      return padT + innerH - ((Math.log10(clamped) - Math.log10(lo)) / (Math.log10(hi) - Math.log10(lo))) * innerH;
    }
    const top = (scale as { top: number }).top;
    return padT + innerH - (v / top) * innerH;
  };

  const path = (points: (number | null)[]) => {
    let d = '';
    let pen = false;
    points.forEach((p, i) => {
      if (p === null || (log && p <= 0)) {
        pen = false;
        return;
      }
      d += `${pen ? 'L' : 'M'}${x(i).toFixed(1)} ${y(p).toFixed(1)} `;
      pen = true;
    });
    return d.trim();
  };

  const labelIdx = (i: number) => Math.round((i / (labels.length - 1)) * (count - 1));

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height }} preserveAspectRatio="none" role="img">
      {(log ? (scale as { ticks: number[] }).ticks : (scale as { ticks: number[] }).ticks).map(t => (
        <g key={t}>
          <line x1={padL} x2={W - padR} y1={y(t)} y2={y(t)} stroke={GRID} strokeWidth="1" opacity="0.6" />
          <text x={padL - 6} y={y(t) + 3} textAnchor="end" fontSize="10" fill={TEXT}>
            {formatY(t)}
          </text>
        </g>
      ))}
      {labels.map((label, i) => (
        <text key={label + i} x={x(labelIdx(i))} y={H - 6} textAnchor={i === 0 ? 'start' : i === labels.length - 1 ? 'end' : 'middle'} fontSize="10" fill={TEXT}>
          {label}
        </text>
      ))}
      {series.map(s => (
        <path key={s.name} d={path(s.points)} fill="none" stroke={s.color} strokeWidth="1.75" strokeLinejoin="round" strokeLinecap="round" vectorEffect="non-scaling-stroke" />
      ))}
    </svg>
  );
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
  const W = 600;
  const H = height;
  const padL = 30;
  const padR = 12;
  const padT = 12;
  const padB = 24;
  const innerW = W - padL - padR;
  const innerH = H - padT - padB;
  const count = Math.max(...series.map(s => s.points.length), 1);
  const { ticks, top } = niceScale(Math.max(...series.flatMap(s => s.points.filter((p): p is number => p !== null)), 0));

  const x = (i: number) => padL + (i / count) * innerW;
  const y = (v: number) => padT + innerH - (v / top) * innerH;

  const stepPath = (points: (number | null)[]) => {
    let d = '';
    points.forEach((p, i) => {
      const value = p ?? 0;
      const px = x(i);
      const py = y(value);
      d += i === 0 ? `M${px.toFixed(1)} ${py.toFixed(1)}` : `L${px.toFixed(1)} ${y(points[i - 1] ?? 0).toFixed(1)} L${px.toFixed(1)} ${py.toFixed(1)}`;
      if (i === points.length - 1) d += `L${x(i + 1).toFixed(1)} ${py.toFixed(1)}`;
    });
    return d;
  };

  const labelIdx = (i: number) => Math.min(Math.round((i / (labels.length - 1)) * count), count - 1);

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height }} preserveAspectRatio="none" role="img">
      {ticks.map(t => (
        <g key={t}>
          <line x1={padL} x2={W - padR} y1={y(t)} y2={y(t)} stroke={GRID} strokeWidth="1" opacity="0.6" />
          <text x={padL - 6} y={y(t) + 3} textAnchor="end" fontSize="10" fill={TEXT}>
            {formatY(t)}
          </text>
        </g>
      ))}
      {labels.map((label, i) => (
        <text key={label + i} x={x(labelIdx(i))} y={H - 6} textAnchor={i === 0 ? 'start' : i === labels.length - 1 ? 'end' : 'middle'} fontSize="10" fill={TEXT}>
          {label}
        </text>
      ))}
      {series.map(s => (
        <path key={s.name} d={stepPath(s.points)} fill="none" stroke={s.color} strokeWidth="1.75" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
      ))}
    </svg>
  );
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
  const W = 600;
  const H = height;
  const padL = 30;
  const padR = 12;
  const padT = 12;
  const padB = 24;
  const innerW = W - padL - padR;
  const innerH = H - padT - padB;
  const { ticks, top } = niceScale(Math.max(...buckets.map(b => b.success + b.failure), 0));
  const slot = innerW / Math.max(buckets.length, 1);
  const barW = Math.max(slot * 0.62, 2);
  const y = (v: number) => padT + innerH - (v / top) * innerH;
  const x = (i: number) => padL + i * slot + slot / 2;
  const labelIdx = (i: number) => Math.min(Math.round((i / (labels.length - 1)) * buckets.length), buckets.length - 1);

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height }} preserveAspectRatio="none" role="img">
      {ticks.map(t => (
        <g key={t}>
          <line x1={padL} x2={W - padR} y1={y(t)} y2={y(t)} stroke={GRID} strokeWidth="1" opacity="0.6" />
          <text x={padL - 6} y={y(t) + 3} textAnchor="end" fontSize="10" fill={TEXT}>
            {formatY(t)}
          </text>
        </g>
      ))}
      {buckets.map((bucket, i) => {
        const success = bucket.success;
        const failure = bucket.failure;
        return (
          <g key={i}>
            <rect x={x(i) - barW / 2} y={y(success)} width={barW} height={Math.max(padT + innerH - y(success), success > 0 ? 1.5 : 0)} fill="#34d399" opacity="0.85" rx="1" />
            {failure > 0 && (
              <rect x={x(i) - barW / 2} y={y(success + failure)} width={barW} height={Math.max(y(success) - y(success + failure), 1.5)} fill="#f87171" opacity="0.9" rx="1" />
            )}
          </g>
        );
      })}
      {labels.map((label, i) => (
        <text key={label + i} x={x(labelIdx(i))} y={H - 6} textAnchor={i === 0 ? 'start' : i === labels.length - 1 ? 'end' : 'middle'} fontSize="10" fill={TEXT}>
          {label}
        </text>
      ))}
    </svg>
  );
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
