import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { defineChart, lineY } from '@tanstack/charts';
import { scaleLinear } from '@tanstack/charts/scales/linear';
import { Chart } from '@tanstack/charts/react';
import { decodePayload, parseAnsi } from '../src/lib/ansi.ts';
import { jobPath } from '../src/lib/routes.ts';

test('terminal hyperlinks are stripped and ANSI styles use only allowed CSS classes', () => {
  const payload = '<img src=x onerror=alert(1)><script>alert(2)</script>';
  const encoded = Buffer.from(`\x1b]8;;javascript:alert(3)\x07\x1b[31m${payload}\x1b[0m\x1b]8;;\x07\x1b[999m tail`).toString('base64');
  assert.deepEqual(parseAnsi(decodePayload(encoded)), [
    { text: payload, classes: ['ansi-red'] },
    { text: ' tail', classes: [] },
  ]);
});

test('job links keep attacker-controlled names in an encoded local path', () => {
  for (const name of ['javascript:alert(1)', '//evil.example/', '"><img src=x onerror=alert(1)>']) {
    const path = jobPath(name);
    const url = new URL(path, 'https://minicron.example');
    assert.equal(url.origin, 'https://minicron.example');
    assert.equal(decodeURIComponent(url.pathname.slice('/jobs/'.length)), name);
  }
});

test('chart SVG serialization escapes API-derived axis labels and attributes', () => {
  const payload = '"><img src=x onerror=alert(1)><script>alert(2)</script>&';
  const rows = [{ index: 0, value: 1, series: payload }, { index: 1, value: 2, series: payload }];
  const definition = defineChart({
    marks: [lineY(rows, { x: 'index', y: 'value', z: 'series', stroke: '#34d399' })],
    scales: {
      x: { scale: scaleLinear().domain([0, 1]), axis: { ticks: { values: [0, 1], format: () => payload } } },
      y: { scale: scaleLinear().domain([0, 2]) },
    },
  });
  const html = renderToStaticMarkup(createElement(Chart, { definition, height: 220, ariaLabel: payload }));
  assert.match(html, /<svg\b/);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /aria-label="&quot;&gt;&lt;img/);
  assert.doesNotMatch(html, /<(?:img|script)\b/i);
  assert.doesNotMatch(html, /\sonerror="/i);
});
