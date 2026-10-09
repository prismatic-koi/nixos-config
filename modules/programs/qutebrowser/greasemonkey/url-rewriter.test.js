'use strict';
// Run: nix shell nixpkgs#nodejs -c node --test modules/programs/qutebrowser/greasemonkey/
const test = require('node:test');
const assert = require('node:assert');
const { rewriteUrl, rewriteSrcset } = require('./url_rewriter.js');

const same = (s) => assert.strictEqual(rewriteSrcset(s), s);

test('data: URL with comma is unchanged', () => {
  same("data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg'></svg>");
});

test('CDN URLs with commas, no rule match, are unchanged', () => {
  same('https://res.cloudinary.com/x/image/upload/w_400,h_300/a.jpg 1x, https://res.cloudinary.com/x/image/upload/w_800,h_600/a.jpg 2x');
});

test('matching rules rewrite every candidate', () => {
  assert.strictEqual(
    rewriteSrcset('https://i0.wp.com/example.com/a.jpg 1x, https://i1.wp.com/example.com/b.jpg 2x'),
    'https://example.com/a.jpg 1x, https://example.com/b.jpg 2x');
});

test('comma inside a rewritten URL is kept', () => {
  assert.strictEqual(
    rewriteSrcset('https://i0.wp.com/example.com/w_400,h_300/a.jpg 1x'),
    'https://example.com/w_400,h_300/a.jpg 1x');
});

test('data: candidate beside a rewritten one is kept', () => {
  assert.strictEqual(
    rewriteSrcset('https://i0.wp.com/example.com/a.jpg 1x, data:image/png;base64,AAAA 2x'),
    'https://example.com/a.jpg 1x, data:image/png;base64,AAAA 2x');
});

test('empty and falsy values pass through', () => {
  assert.strictEqual(rewriteSrcset(''), '');
  assert.strictEqual(rewriteSrcset(undefined), undefined);
});

test('rewriteUrl (src/href path) is unchanged', () => {
  assert.strictEqual(rewriteUrl('https://i2.wp.com/example.com/a.jpg'), 'https://example.com/a.jpg');
  assert.strictEqual(rewriteUrl('https://example.com/a.jpg'), 'https://example.com/a.jpg');
  assert.strictEqual(rewriteUrl(''), '');
});
