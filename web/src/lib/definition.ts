import type { Definition } from '../types';

export function emptyDefinition(kind: 'job' | 'worker' = 'job'): Definition {
  const base: Definition = {
    name: '',
    kind,
    enabled: true,
    timezone: 'UTC',
    env_base: 'clean',
    timeout: 0,
    grace: 10,
  };
  if (kind === 'job') {
    base.schedule = '0 0 * * *';
    base.catch_up = 'none';
    base.on_overlap = 'skip';
    base.retries = 0;
    base.retry_delay = 5;
    base.run_on_start = false;
  } else {
    base.autostart = true;
    base.restart = 'always';
    base.restart_delay = 5;
    base.max_restart_attempts = 5;
    base.healthy_after = 30;
  }
  return base;
}

export function updateDefinitionDraft(draft: Definition, changes: Partial<Definition>): Definition {
  if (changes.kind && changes.kind !== draft.kind) {
    return { ...emptyDefinition(changes.kind), name: draft.name };
  }
  return { ...draft, ...changes };
}

export function duplicateDefinition(definition: Definition): Definition {
  const draft = structuredClone(definition);
  draft.name = '';
  delete draft.revision;
  delete draft.definition_id;
  delete draft.source;
  delete draft.next_fire_at;
  return draft;
}
