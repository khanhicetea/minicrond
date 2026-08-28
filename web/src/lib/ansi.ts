/**
 * Sanitize ANSI escape sequences into renderable segments.
 *
 * Mirrors the daemon-side render contract: OSC sequences and C0 control
 * characters are stripped; only a small allow-list of SGR codes (bold and the
 * eight base colors) is honored; every other CSI sequence is removed.
 */
export interface AnsiSegment {
  text: string;
  classes: string[];
}

const SGR_CLASSES: Record<number, string> = {
  1: 'ansi-bold',
  31: 'ansi-red',
  32: 'ansi-green',
  33: 'ansi-yellow',
  34: 'ansi-blue',
  35: 'ansi-magenta',
  36: 'ansi-cyan',
};

/** Remove any remaining (non-SGR) CSI sequences from a text slice. */
function stripCsi(text: string): string {
  return text.replace(/\x1b\[[0-?]*[ -/]*[@-~]/g, '');
}

export function parseAnsi(input: string): AnsiSegment[] {
  const text = input
    .replace(/\x1b\][^\x07]*(?:\x07|\x1b\\)/g, '') // OSC sequences
    .replace(/[\x00-\x08\x0b-\x1a\x1c-\x1f\x7f]/g, ''); // control chars except \t \n \x1b

  const segments: AnsiSegment[] = [];
  let open: string[] = [];
  let last = 0;
  for (const match of text.matchAll(/\x1b\[([0-9;]*)m/g)) {
    const before = stripCsi(text.slice(last, match.index));
    if (before) segments.push({ text: before, classes: [...open] });
    const codes = match[1].split(';').map(Number);
    if (codes.includes(0)) {
      open = [];
    } else {
      const allowed = codes.map(code => SGR_CLASSES[code]).filter(Boolean);
      if (allowed.length) open = [...open, ...allowed];
    }
    last = match.index + match[0].length;
  }
  const rest = stripCsi(text.slice(last));
  if (rest) segments.push({ text: rest, classes: [...open] });
  return segments;
}

/** Decode a base64 log-frame payload as lossy UTF-8. */
export function decodePayload(encoded: string): string {
  const raw = atob(encoded);
  const bytes = Uint8Array.from(raw, char => char.charCodeAt(0));
  return new TextDecoder('utf-8', { fatal: false }).decode(bytes);
}
