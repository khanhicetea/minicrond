/**
 * Suggest direct execution only for shell strings whose words can be copied
 * into the form's one-argument-per-line argv editor without changing them.
 * This is deliberately a small subset of shell syntax, not a shell parser.
 */
export function commandToArgv(command: string): string[] | null {
  const argv: string[] = [];
  let word = '';
  let hasWord = false;
  let quote: 'single' | 'double' | null = null;

  for (const char of command) {
    if (quote === 'single') {
      if (char === "'") quote = null;
      else if (char === '\n' || char === '\r') return null;
      else word += char;
      continue;
    }
    if (quote === 'double') {
      if (char === '"') quote = null;
      else if (char === '$' || char === '`' || char === '\\' || char === '\n' || char === '\r') return null;
      else word += char;
      continue;
    }
    if (char === "'") {
      quote = 'single';
      hasWord = true;
    } else if (char === '"') {
      quote = 'double';
      hasWord = true;
    } else if (char === ' ' || char === '\t') {
      if (hasWord) {
        if (!word || word.trim() !== word) return null;
        argv.push(word);
        word = '';
        hasWord = false;
      }
    } else if (/[a-zA-Z0-9_./:@%+=,-]/.test(char)) {
      word += char;
      hasWord = true;
    } else {
      return null;
    }
  }

  if (quote || (hasWord && (!word || word.trim() !== word))) return null;
  if (hasWord) argv.push(word);
  // The direct executor accepts absolute paths, and this avoids any change
  // from shell PATH lookup to the daemon's trusted-path lookup.
  if (argv.length === 0 || !argv[0].startsWith('/')) return null;
  return argv;
}
