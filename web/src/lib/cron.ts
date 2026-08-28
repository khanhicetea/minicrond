/**
 * Minimal five-field cron evaluator + @every descriptor support, used for
 * next-fire countdowns (dashboard "Up next"), the editor cron helper, and
 * humanized schedule descriptions. Mirrors the daemon's field semantics:
 * minute hour day-of-month month day-of-week, with the standard rule that a
 * restricted day-of-month OR day-of-week matches when the other is "*".
 */

export interface CronFields {
  minute: number[];
  hour: number[];
  dom: number[];
  month: number[];
  dow: number[]; // 0 = Sunday, supports 7 as Sunday
}

export type ParsedSchedule =
  | { kind: 'cron'; fields: CronFields; domRestricted: boolean; dowRestricted: boolean }
  | { kind: 'every'; ms: number };

const MONTH_NAMES = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const DOW_NAMES = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];

function parseField(field: string, min: number, max: number, names?: string[]): number[] | null {
  const values = new Set<number>();
  for (const part of field.split(',')) {
    let body = part;
    let step = 1;
    const slash = part.indexOf('/');
    if (slash >= 0) {
      body = part.slice(0, slash);
      step = Number(part.slice(slash + 1));
      if (!Number.isInteger(step) || step < 1) return null;
    }
    let start: number;
    let end: number;
    if (body === '*') {
      start = min;
      end = max;
    } else if (body === '') {
      return null;
    } else if (body.includes('-')) {
      const [a, b] = body.split('-');
      const from = resolveValue(a, names);
      const to = resolveValue(b, names);
      if (from === null || to === null) return null;
      start = from;
      end = to;
    } else {
      const v = resolveValue(body, names);
      if (v === null) return null;
      if (slash >= 0) {
        start = v;
        end = max;
      } else {
        if (v < min || v > max) return null;
        values.add(v === 7 && max === 6 ? 0 : v); // dow 7 == 0
        continue;
      }
    }
    if (start < min || end > max || start > end) return null;
    for (let v = start; v <= end; v += step) values.add(v === 7 && max === 6 ? 0 : v);
  }
  if (values.size === 0) return null;
  return [...values].sort((a, b) => a - b);
}

function resolveValue(text: string, names?: string[]): number | null {
  if (/^\d+$/.test(text)) return Number(text);
  if (names) {
    const index = names.indexOf(text.toLowerCase());
    if (index >= 0) return index;
  }
  return null;
}

const DESCRIPTORS: Record<string, string> = {
  '@hourly': '0 * * * *',
  '@daily': '0 0 * * *',
  '@midnight': '0 0 * * *',
  '@weekly': '0 0 * * 0',
  '@monthly': '0 0 1 * *',
  '@yearly': '0 0 1 1 *',
  '@annually': '0 0 1 1 *',
};

/** Parse a Go-style duration ("90s", "20m", "1h30m", "500ms", "0"). */
export function parseDuration(text: string): number | null {
  const value = text.trim();
  if (!value) return null;
  if (/^0+$/.test(value)) return 0; // explicit zero (e.g. timeout "0" = none)
  const match = value.match(/^(\d+(?:\.\d+)?(?:ms|us|µs|ns|[smh]))+$/);
  if (!match) return null;
  let ms = 0;
  for (const part of value.match(/\d+(?:\.\d+)?(?:ms|us|µs|ns|[smh])/g) ?? []) {
    const amount = Number(part.slice(0, -1));
    const unit = part.slice(-1);
    if (part.endsWith('ms')) ms += Number(part.slice(0, -2));
    else if (unit === 's') ms += amount * 1000;
    else if (unit === 'm') ms += amount * 60_000;
    else if (unit === 'h') ms += amount * 3_600_000;
    else return null; // us/ns not worth supporting in the UI
  }
  return ms >= 0 ? ms : null;
}

export function parseSchedule(expr: string): ParsedSchedule | null {
  const text = expr.trim().toLowerCase();
  if (!text) return null;
  if (text.startsWith('@every')) {
    const ms = parseDuration(text.slice(6));
    return ms ? { kind: 'every', ms } : null;
  }
  const expanded = DESCRIPTORS[text] ?? text;
  const parts = expanded.split(/\s+/);
  if (parts.length !== 5) return null;
  const minute = parseField(parts[0], 0, 59);
  const hour = parseField(parts[1], 0, 23);
  const dom = parseField(parts[2], 1, 31);
  const month = parseField(parts[3], 1, 12, MONTH_NAMES);
  const dow = parseField(parts[4], 0, 7, DOW_NAMES);
  if (!minute || !hour || !dom || !month || !dow) return null;
  return {
    kind: 'cron',
    fields: { minute, hour, dom, month, dow },
    domRestricted: parts[2] !== '*',
    dowRestricted: parts[4] !== '*',
  };
}

/* ---------- timezone helpers ---------- */

interface TzParts {
  year: number;
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

const formatterCache = new Map<string, Intl.DateTimeFormat>();
const partsCache = new Map<string, { key: number; parts: TzParts }>();

function tzFormatter(tz: string): Intl.DateTimeFormat | null {
  let formatter = formatterCache.get(tz);
  if (!formatter) {
    try {
      formatter = new Intl.DateTimeFormat('en-US', {
        timeZone: tz,
        hour12: false,
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      });
    } catch {
      return null; // unknown timezone
    }
    formatterCache.set(tz, formatter);
  }
  return formatter;
}

export function isValidTimezone(tz: string): boolean {
  return tzFormatter(tz) !== null;
}

function wallParts(tz: string, ms: number): TzParts | null {
  const cached = partsCache.get(tz);
  if (cached && cached.key === ms) return cached.parts;
  const formatter = tzFormatter(tz);
  if (!formatter) return null;
  const map: Record<string, string> = {};
  for (const part of formatter.formatToParts(new Date(ms))) map[part.type] = part.value;
  const parts: TzParts = {
    year: Number(map.year),
    month: Number(map.month),
    day: Number(map.day),
    hour: map.hour === '24' ? 0 : Number(map.hour),
    minute: Number(map.minute),
    second: Number(map.second),
  };
  partsCache.set(tz, { key: ms, parts });
  return parts;
}

/** Convert wall-clock time in a timezone to a UTC instant (DST-aware). */
function wallToMs(tz: string, year: number, month: number, day: number, hour: number, minute: number): number {
  // Two-pass correction: find the UTC offset the zone applies near this
  // wall time, then subtract it. Handles DST gaps and folds reasonably.
  const asUtc = Date.UTC(year, month - 1, day, hour, minute, 0);
  let instant = asUtc;
  for (let i = 0; i < 2; i += 1) {
    const parts = wallParts(tz, instant);
    if (!parts) break;
    const rendered = Date.UTC(parts.year, parts.month - 1, parts.day, parts.hour, parts.minute);
    instant = asUtc - (rendered - instant);
  }
  return instant;
}

function daysInMonth(year: number, month: number): number {
  return new Date(Date.UTC(year, month, 0)).getUTCDate();
}

function dayOfWeek(year: number, month: number, day: number): number {
  return new Date(Date.UTC(year, month - 1, day)).getUTCDay();
}

function matchesDay(fields: CronFields, domRestricted: boolean, dowRestricted: boolean, year: number, month: number, day: number): boolean {
  const domOk = fields.dom.includes(day);
  const dowOk = fields.dow.includes(dayOfWeek(year, month, day));
  if (domRestricted && dowRestricted) return domOk || dowOk;
  if (domRestricted) return domOk;
  if (dowRestricted) return dowOk;
  return true;
}

const MAX_STEPS = 200_000;

/** Next fire time (UTC ms) strictly after `afterMs`, or null when unschedulable. */
export function nextFire(expr: string, afterMs: number, tz = 'UTC'): number | null {
  const parsed = parseSchedule(expr);
  if (!parsed || !isValidTimezone(tz)) return null;
  if (parsed.kind === 'every') return afterMs + parsed.ms;

  const { fields, domRestricted, dowRestricted } = parsed;
  const start = wallParts(tz, afterMs);
  if (!start) return null;

  let year = start.year;
  let month = start.month;
  let day = start.day;
  let hour = start.hour;
  let minute = start.minute + 1; // strictly after

  for (let step = 0; step < MAX_STEPS; step += 1) {
    if (minute > 59) {
      minute = 0;
      hour += 1;
    }
    if (hour > 23) {
      hour = 0;
      day += 1;
    }
    if (day > daysInMonth(year, month)) {
      day = 1;
      month += 1;
    }
    if (month > 12) {
      month = 1;
      year += 1;
    }
    if (!fields.month.includes(month)) {
      month += 1;
      day = 1;
      hour = 0;
      minute = 0;
      continue;
    }
    if (!matchesDay(fields, domRestricted, dowRestricted, year, month, day)) {
      day += 1;
      hour = 0;
      minute = 0;
      continue;
    }
    if (!fields.hour.includes(hour)) {
      hour += 1;
      minute = 0;
      continue;
    }
    if (!fields.minute.includes(minute)) {
      minute += 1;
      continue;
    }
    return wallToMs(tz, year, month, day, hour, minute);
  }
  return null; // e.g. Feb 30 — unschedulable
}

/** Next `count` fire times starting strictly after `afterMs`. */
export function nextFires(expr: string, count: number, afterMs: number, tz = 'UTC'): number[] {
  const fires: number[] = [];
  let cursor = afterMs;
  for (let i = 0; i < count; i += 1) {
    const next = nextFire(expr, cursor, tz);
    if (next === null) break;
    fires.push(next);
    cursor = next;
  }
  return fires;
}

/* ---------- humanizing ---------- */

function two(n: number): string {
  return String(n).padStart(2, '0');
}

function clockLabel(hour: number, minute: number): string {
  return `${two(hour)}:${two(minute)}`;
}

function durationWords(ms: number): string {
  if (ms % 3_600_000 === 0) {
    const hours = ms / 3_600_000;
    return `${hours}h`;
  }
  if (ms % 60_000 === 0) {
    const minutes = ms / 60_000;
    return `${minutes}m`;
  }
  const seconds = Math.round(ms / 1000);
  return `${seconds}s`;
}

function weekdayName(dow: number): string {
  return ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'][dow] ?? '';
}

function monthName(month: number): string {
  return ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'][month - 1] ?? '';
}

function joinList(items: string[]): string {
  if (items.length <= 1) return items[0] ?? '';
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`;
}

/**
 * Short human description of a schedule, e.g. "Every 5 minutes",
 * "Daily at 02:00", "Every Monday at 02:00". Falls back to the raw
 * expression when no pattern matches.
 */
export function humanizeSchedule(expr: string): string {
  const text = expr.trim();
  if (!text) return 'Not scheduled';
  const lowered = text.toLowerCase();
  if (lowered.startsWith('@every')) {
    const ms = parseDuration(lowered.slice(6));
    return ms ? `Every ${durationWords(ms)}` : text;
  }
  const expanded = DESCRIPTORS[lowered] ?? lowered;
  const parts = expanded.split(/\s+/);
  if (parts.length !== 5) return text;
  const [m, h, dom, mon, dow] = parts;

  const minuteList = parseField(m, 0, 59);
  const hourList = parseField(h, 0, 23);
  if (!minuteList || !hourList) return text;
  const domAny = dom === '*';
  const monAny = mon === '*';
  const dowAny = dow === '*';

  if (m.startsWith('*/') && /^\d+$/.test(m.slice(2)) && h === '*' && domAny && monAny && dowAny) {
    return `Every ${durationWords(Number(m.slice(2)) * 60_000)}`;
  }
  if (h === '*' && m === '*' && domAny && monAny && dowAny) return 'Every hour';
  if (h === '*' && domAny && monAny && dowAny && minuteList.length === 1) {
    return `Every hour at :${two(minuteList[0])}`;
  }
  if (minuteList.length === 1 && hourList.length === 1 && domAny && monAny && dowAny) {
    return `Daily at ${clockLabel(hourList[0], minuteList[0])}`;
  }
  if (minuteList.length === 1 && hourList.length === 1 && domAny && monAny && !dowAny) {
    const days = parseField(dow, 0, 7, DOW_NAMES);
    if (!days) return text;
    const names = days.map(d => weekdayName(d === 7 ? 0 : d));
    if (days.length === 7) return `Daily at ${clockLabel(hourList[0], minuteList[0])}`;
    return `${joinList(names)} at ${clockLabel(hourList[0], minuteList[0])}`;
  }
  if (minuteList.length === 1 && hourList.length === 1 && dowAny && monAny && !domAny) {
    const days = parseField(dom, 1, 31);
    if (!days) return text;
    const ordinal = (d: number) => {
      const s = ['th', 'st', 'nd', 'rd'];
      const v = d % 100;
      return `${d}${s[(v - 20) % 10] ?? s[v] ?? s[0]}`;
    };
    if (days.length === 1 && days[0] === 1) return `Monthly on the 1st at ${clockLabel(hourList[0], minuteList[0])}`;
    return `Monthly on the ${joinList(days.map(ordinal))} at ${clockLabel(hourList[0], minuteList[0])}`;
  }
  if (minuteList.length === 1 && hourList.length === 1 && domAny && dowAny && !monAny) {
    const months = parseField(mon, 1, 12, MONTH_NAMES);
    if (!months) return text;
    return `${joinList(months.map(monthName))} at ${clockLabel(hourList[0], minuteList[0])}`;
  }
  return text;
}

/**
 * Build a cron expression from five component fields. Fields may be "*",
 * "n", "*\/n", "a-b" or "a-b\/n".
 */
export function buildCron(minute: string, hour: string, dom: string, month: string, dow: string): string {
  return `${minute || '*'} ${hour || '*'} ${dom || '*'} ${month || '*'} ${dow || '*'}`;
}

/** Extract simple single values from a cron expression for the helper selects. */
export function cronSelectValues(expr: string): [string, string, string, string, string] {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return ['*', '*', '*', '*', '*'];
  return [parts[0], parts[1], parts[2], parts[3], parts[4]];
}
