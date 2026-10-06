import type { ProcessSample } from '../types';

/** Interval CPU: 100% means one fully occupied logical CPU. */
export function cpuPercent(previous: ProcessSample | null | undefined, current: ProcessSample | null | undefined): number | undefined {
  if (!previous || !current || previous.process_start_id !== current.process_start_id) return undefined;
  const elapsedUS = (Date.parse(current.sampled_at) - Date.parse(previous.sampled_at)) * 1000;
  const cpuUS = current.cpu_us - previous.cpu_us;
  if (!Number.isFinite(elapsedUS) || elapsedUS <= 0 || cpuUS < 0) return undefined;
  return cpuUS / elapsedUS * 100;
}

export function memoryMiB(bytes: number | null | undefined): string {
  return bytes == null ? '—' : `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}
