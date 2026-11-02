import assert from 'node:assert/strict';
import { readdirSync, readFileSync } from 'node:fs';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';

function sources(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const path = `${dir}/${entry.name}`;
    return entry.isDirectory() ? sources(path) : /\.tsx?$/.test(path) ? [path] : [];
  });
}

// Conservative source guardrails, not a taint-analysis/security proof. New URL
// rendering patterns need an explicit review; don't blanket-allow API fields.
function violations(text, file) {
  const source = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const errors = [];
  const isCall = (node, name) => node && ts.isCallExpression(node) && node.expression.getText(source) === name;
  const literal = node => node && ts.isStringLiteral(node);
  const local = value => value.startsWith('/') && !value.startsWith('//') && !value.includes('\\');
  function safeRoute(node) {
    if (literal(node)) return local(node.text);
    if (isCall(node, 'jobPath')) return true; // Encodes the untrusted name; security.test.mjs.
    if (ts.isConditionalExpression(node)) return safeRoute(node.whenTrue) && safeRoute(node.whenFalse);
    if (ts.isTemplateExpression(node)) {
      return local(node.head.text) && node.templateSpans.every(span => isCall(span.expression, 'encodeURIComponent'));
    }
    // This component's navigation items are code-owned constants, not API data.
    return file.endsWith('/components/Layout.tsx') && node.getText(source) === 'item.href';
  }
  function visit(node) {
    const fail = message => errors.push(`${file}:${source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1}: ${message}`);
    if (ts.isIdentifier(node) || ts.isStringLiteral(node)) {
      if (['dangerouslySetInnerHTML', 'innerHTML', 'outerHTML', 'insertAdjacentHTML'].includes(node.text)) fail('no HTML sinks');
    }
    if (ts.isCallExpression(node)) {
      const callee = node.expression.getText(source);
      if (callee === 'fetch') {
        if (!file.endsWith('/src/api.ts') || !isCall(node.arguments[0], 'apiPath')) fail('fetch must use api.ts and apiPath');
      }
      if (['document.write', 'document.writeln', 'navigator.sendBeacon'].includes(callee)) fail('unexpected HTML/network sink');
      if (callee === 'downloadFile' && !literal(node.arguments[1])) fail('download filename must be fixed');
      if (callee === 'request') {
        const path = node.arguments[0];
        const prefix = literal(path) ? path.text : ts.isTemplateExpression(path) ? path.head.text : '';
        if (!prefix.startsWith('/api/v1/')) fail('request path must start with /api/v1/');
      }
    }
    if (ts.isNewExpression(node) && ['XMLHttpRequest', 'WebSocket', 'EventSource'].includes(node.expression.getText(source))) fail('network must use api.ts');
    if (ts.isJsxAttribute(node) && ['href', 'src', 'download'].includes(node.name.getText(source))) {
      const value = node.initializer;
      const expression = value && ts.isJsxExpression(value) ? value.expression : value;
      if (node.name.getText(source) === 'download' ? !literal(expression) : !expression || !safeRoute(expression)) fail('URL must be a code-owned local route with encoded fields; downloads must have fixed names');
    }
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.EqualsToken && ts.isPropertyAccessExpression(node.left)) {
      const property = node.left.name.text;
      if (['href', 'src'].includes(property) && !isCall(node.right, 'URL.createObjectURL')) fail('DOM URLs may only use locally created download blobs');
      if (property === 'download' && !literal(node.right) && !(file.endsWith('/lib/download.ts') && node.right.getText(source) === 'filename')) fail('download filename must be fixed');
    }
    ts.forEachChild(node, visit);
  }
  visit(source);
  return errors;
}

test('SPA networking and rendering obey the gateway trust boundary', () => {
  for (const file of sources(fileURLToPath(new URL('../src', import.meta.url)))) {
    const text = readFileSync(file, 'utf8');
    assert.doesNotMatch(text, /\/(?:healthz|readyz|openapi\.json)\b/, file);
    assert.deepEqual(violations(text, file), []);
  }
});

test('source guardrails reject unsafe examples', () => {
  for (const text of [
    '<div dangerouslySetInnerHTML={{ __html: data.message }} />',
    'element.innerHTML = data.message',
    'element["innerHTML"] = data.message',
    '<a href={data.url}>link</a>',
    '<img src={data.url} />',
    '<a download={data.name} />',
    'link.href = data.url',
    'link.download = data.name',
    'downloadFile(url, data.name, api.download)',
    'fetch("/healthz")',
    'request("/readyz")',
    'new EventSource("/events")',
  ]) assert.notEqual(violations(text, '/src/unsafe.tsx').length, 0, text);
});

test('API URL confinement preserves BASE_PATH and rejects escapes', async () => {
  globalThis.window = { location: { origin: 'https://gateway.example' } };
  for (const base of ['/', '/tools/minicron/']) {
    globalThis.document = { querySelector: () => ({ getAttribute: () => base }) };
    const { apiPath } = await import(`../src/lib/base.ts?base=${encodeURIComponent(base)}`);
    assert.equal(apiPath('/api/v1/daemon'), `${base}api/v1/daemon`);
    assert.equal(apiPath('/api/v1/export?format=json'), `${base}api/v1/export?format=json`);
    for (const path of ['/healthz', '/readyz', '/openapi.json', '/api/v10/jobs', '//evil.example/api/v1/jobs', 'https://evil.example/api/v1/jobs', '/api/v1/../../jobs', '/api/v1/%2e%2e/%2e%2e/jobs', '/api/v1/..\\..\\jobs']) {
      assert.throws(() => apiPath(path), /UI requests must stay under/);
    }
  }
});
