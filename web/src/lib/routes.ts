export function jobPath(name: string): string {
  return `/jobs/${encodeURIComponent(name)}`;
}
