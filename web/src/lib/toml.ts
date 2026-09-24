import type { Definition } from '../types';

/**
 * Serialize a definition as minicron TOML ([[job]] / [[worker]]), matching the
 * daemon config format. Returns the text plus a field→line map so validation
 * issues can point at the offending line.
 */

function literal(text: string): string {
  if (!text.includes("'") && !text.includes('\n') && !text.includes('\u0000')) {
    return `'${text}'`;
  }
  return `"${text.replace(/\\/g, '\\\\').replace(/"/g, '\\"').replace(/\n/g, '\\n')}"`;
}


export function definitionToToml(def: Definition): { text: string; lineOf: Record<string, number> } {
  const isWorker = def.kind === 'worker';
  const header = isWorker ? 'worker' : 'job';
  const lines: string[] = [];
  const lineMap: Record<string, number> = {};

  const emit = (field: string, key: string, value: string) => {
    lineMap[field] = lines.length + 1;
    lines.push(`${key} = ${value}`);
  };

  lines.push(`[[${header}]]`);
  if (def.name) emit('name', 'name', literal(def.name));
  if (def.command) emit('command', 'command', literal(def.command));
  if (def.argv && def.argv.length > 0) {
    lineMap.argv = lines.length + 1;
    lines.push(`argv = [${def.argv.map(literal).join(', ')}]`);
  }
  if (def.shell) emit('shell', 'shell', literal(def.shell));
  if (isWorker) {
    if (def.autostart !== undefined) emit('autostart', 'autostart', String(def.autostart));
    if (def.restart) emit('restart', 'restart', literal(def.restart));
    if (def.restart_delay !== undefined) emit('restart_delay', 'restart_delay', String(def.restart_delay));
    if (def.max_restart_attempts) emit('max_restart_attempts', 'max_restart_attempts', String(def.max_restart_attempts));
    if (def.healthy_after !== undefined) emit('healthy_after', 'healthy_after', String(def.healthy_after));
    if (def.priority) emit('priority', 'priority', String(def.priority));
  } else {
    if (def.schedule) emit('schedule', 'schedule', literal(def.schedule));
    if (def.catch_up) emit('catch_up', 'catch_up', literal(def.catch_up));
    if (def.on_overlap) emit('on_overlap', 'on_overlap', literal(def.on_overlap));
    if (def.run_on_start) emit('run_on_start', 'run_on_start', 'true');
  }
  if (def.timezone) emit('timezone', 'timezone', literal(def.timezone));
  if (def.run_as) emit('run_as', 'run_as', literal(def.run_as));
  if (def.working_dir) emit('working_dir', 'working_dir', literal(def.working_dir));
  if (def.env_base && def.env_base !== 'clean') emit('env_base', 'env_base', literal(def.env_base));
  if (def.timeout !== undefined) emit('timeout', 'timeout', String(def.timeout));
  if (def.grace !== undefined) emit('grace', 'grace', String(def.grace));
  if (def.stop_signal) emit('stop_signal', 'stop_signal', literal(def.stop_signal));
  if (def.success_codes && def.success_codes.length > 0) {
    lineMap.success_codes = lines.length + 1;
    lines.push(`success_codes = [${def.success_codes.join(', ')}]`);
  }
  if (def.keep_runs) emit('keep_runs', 'keep_runs', String(def.keep_runs));
  if (def.keep_for) emit('keep_for', 'keep_for', String(def.keep_for));
  if (def.log_max !== undefined) emit('log_max', 'log_max', String(def.log_max));
  if (def.log_on_full) emit('log_on_full', 'log_on_full', literal(def.log_on_full));
  if (def.alerts && def.alerts.length > 0) {
    lineMap.alerts = lines.length + 1;
    lines.push(`alerts = [${def.alerts.map(literal).join(', ')}]`);
  }

  if (def.env && Object.keys(def.env).length > 0) {
    lines.push('');
    lines.push(`[${header}.env]`);
    for (const [key, value] of Object.entries(def.env)) {
      lineMap.env = lineMap.env ?? lines.length + 1;
      lines.push(`${/^[A-Za-z0-9_]+$/.test(key) ? key : literal(key)} = ${literal(value)}`);
    }
  }
  if (def.secret_env && Object.keys(def.secret_env).length > 0) {
    lines.push('');
    lines.push(`[${header}.secret_env]`);
    for (const [key, value] of Object.entries(def.secret_env)) {
      lineMap.secret_env = lineMap.secret_env ?? lines.length + 1;
      lines.push(`${/^[A-Za-z0-9_]+$/.test(key) ? key : literal(key)} = ${literal(value)}`);
    }
  }
  if (def.env_file) emit('env_file', 'env_file', literal(def.env_file));
  if (def.labels && Object.keys(def.labels).length > 0) {
    lines.push('');
    lines.push(`[${header}.labels]`);
    for (const [key, value] of Object.entries(def.labels)) {
      lineMap.labels = lineMap.labels ?? lines.length + 1;
      lines.push(`${/^[A-Za-z0-9_-]+$/.test(key) ? key : literal(key)} = ${literal(value)}`);
    }
  }
  return { text: `${lines.join('\n')}\n`, lineOf: lineMap };
}

/** Best-effort syntax highlighting segments for one TOML line. */
export function highlightToml(line: string): { text: string; cls: string }[] {
  const trimmed = line.trimStart();
  if (trimmed.startsWith('#')) return [{ text: line, cls: 'toml-comment' }];
  const section = line.match(/^(\s*)(\[\[?[^\]]+\]?\])(.*)$/);
  if (section) {
    return [
      { text: section[1], cls: '' },
      { text: section[2], cls: 'toml-section' },
      { text: section[3], cls: '' },
    ];
  }
  const kv = line.match(/^(\s*)([A-Za-z0-9_"'-]+)(\s*=\s*)(.*)$/);
  if (kv) {
    const rest = kv[4];
    const cls = /^["']/.test(rest.trim()) ? 'toml-string' : /^[-+]?[0-9]/.test(rest.trim()) ? 'toml-number' : /^true|false/.test(rest.trim()) ? 'toml-number' : '';
    return [
      { text: `${kv[1]}${kv[2]}`, cls: 'toml-key' },
      { text: kv[3], cls: '' },
      { text: rest, cls },
    ];
  }
  return [{ text: line, cls: '' }];
}
