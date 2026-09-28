import assert from 'node:assert/strict';
import { test } from 'node:test';
import { commandToArgv } from '../src/lib/command-argv.ts';

test('converts an absolute command and simple quoted arguments', () => {
  assert.deepEqual(commandToArgv(' /usr/bin/printf "hello world" --name=\'some value\' '),
    ['/usr/bin/printf', 'hello world', '--name=some value']);
});

test('does not suggest conversion when shell interpretation is needed', () => {
  for (const command of [
    'printf hello',
    '/usr/bin/printf "$HOME"',
    '/usr/bin/printf *.txt',
    '/usr/bin/printf hello | cat',
    '/usr/bin/printf hello > out',
    '/usr/bin/printf hello; true',
    '/usr/bin/printf "unterminated',
    '/usr/bin/printf " padded "',
    '/usr/bin/printf ""',
    '/usr/bin/printf hello\\ world',
  ]) {
    assert.equal(commandToArgv(command), null, command);
  }
});
