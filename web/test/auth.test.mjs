import assert from 'node:assert/strict';
import { test } from 'node:test';

const values = new Map([['minicron_token', 'retained-token']]);
globalThis.sessionStorage = {
  getItem: key => values.get(key) ?? null,
  setItem: (key, value) => values.set(key, value),
  removeItem: key => values.delete(key),
};

const { auth } = await import('../src/auth.ts');

test('proxy access discards a retained bearer and revocation ends access', () => {
  assert.equal(auth.mode, 'checking');
  assert.equal(auth.token, '');
  auth.useProxy();
  assert.equal(auth.mode, 'proxy');
  assert.equal(auth.token, '');
  assert.equal(values.has('minicron_token'), false);
  auth.revoke();
  assert.equal(auth.mode, 'revoked');
});

test('a 401 probe falls back to token login and token 401 clears it', () => {
  auth.requireToken();
  assert.equal(auth.mode, 'login');
  auth.login('fresh-token');
  assert.equal(auth.mode, 'token');
  assert.equal(auth.token, 'fresh-token');
  auth.revoke();
  assert.equal(auth.mode, 'login');
  assert.equal(auth.token, '');
  assert.equal(values.has('minicron_token'), false);
});
