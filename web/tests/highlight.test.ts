import assert from 'node:assert/strict';
import test from 'node:test';
import { parsePatch } from '../src/lib/diff';
import { LANGUAGE_IDS, languageFor, loadTokenizer, mapTokensToRows, tokenStyle, type SideTokenizer, type Token } from '../src/lib/highlight';

/** Splits each line into word/non-word runs tagged with the side text they came from. */
function fakeTokenizer(): { calls: string[]; tokenize: SideTokenizer } {
  const calls: string[] = [];
  return {
    calls,
    tokenize(code) {
      const call = calls.push(code) - 1;
      return code.split('\n').map((line, n) => (line.match(/\w+|\W+/g) ?? []).map((text) => ({ text, color: `c${call}:${n}` })));
    },
  };
}

const texts = (tokens: readonly Token[] | undefined) => tokens?.map((t) => t.text).join('');
const colors = (tokens: readonly Token[] | undefined) => [...new Set(tokens?.map((t) => t.color))];

test('del rows use old-side tokens; add and context rows use new-side tokens', () => {
  const rows = parsePatch('@@ -1,4 +1,4 @@\n a = 1\n-b = "x\n+b = \'y\n+c\tz  \n d\n');
  const fake = fakeTokenizer();
  const tokens = mapTokensToRows(rows, fake.tokenize);
  assert.deepEqual(fake.calls, ['a = 1\nb = "x\nd', 'a = 1\nb = \'y\nc\tz  \nd']);
  for (const [index, row] of rows.entries()) {
    if (row.kind === 'hunk') assert.equal(tokens.has(index), false);
    else assert.equal(texts(tokens.get(index)), row.text, `row ${index} text is preserved exactly`);
  }
  assert.deepEqual(colors(tokens.get(1)), ['c1:0'], 'context takes the new side');
  assert.deepEqual(colors(tokens.get(2)), ['c0:1'], 'del takes old line 1');
  assert.deepEqual(colors(tokens.get(3)), ['c1:1'], 'add takes new line 1');
  assert.deepEqual(colors(tokens.get(4)), ['c1:2']);
  assert.deepEqual(colors(tokens.get(5)), ['c1:3'], 'trailing context stays aligned after an unequal replacement');
});

test('grammar state resets per hunk with one call per non-empty side', () => {
  const rows = parsePatch('@@ -1,2 +1,2 @@\n one\n-two\n+TWO\n@@ -10,1 +10,2 @@\n ten\n+eleven\n@@ -20,1 +21,0 @@\n-gone\n');
  const fake = fakeTokenizer();
  const tokens = mapTokensToRows(rows, fake.tokenize);
  assert.deepEqual(fake.calls, ['one\ntwo', 'one\nTWO', 'ten', 'ten\neleven', 'gone']);
  assert.deepEqual(colors(tokens.get(5)), ['c3:0']);
  assert.deepEqual(colors(tokens.get(6)), ['c3:1']);
  assert.deepEqual(colors(tokens.get(8)), ['c4:0']);
});

test('note rows are never tokenized and do not shift alignment', () => {
  const rows = parsePatch('@@ -1,2 +1,2 @@\n keep\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n');
  const fake = fakeTokenizer();
  const tokens = mapTokensToRows(rows, fake.tokenize);
  assert.deepEqual(fake.calls, ['keep\nold', 'keep\nnew']);
  assert.equal(tokens.has(3), false);
  assert.equal(tokens.has(5), false);
  assert.deepEqual(colors(tokens.get(2)), ['c0:1']);
  assert.deepEqual(colors(tokens.get(4)), ['c1:1']);
});

test('rows whose tokens do not reproduce their text fall back to plain text', () => {
  const rows = parsePatch('@@ -1,2 +1,2 @@\n good\n-bad\n+fine\n');
  const tokens = mapTokensToRows(rows, (code) => code.split('\n').map((line) => [{ text: line === 'bad' ? 'ba' : line, color: 'x' }]));
  assert.equal(tokens.has(2), false);
  assert.equal(texts(tokens.get(1)), 'good');
  assert.equal(texts(tokens.get(3)), 'fine');
});

test('tokenizer failures, line-count mismatches and overlong lines leave rows plain', () => {
  const rows = parsePatch('@@ -1,1 +1,1 @@\n-a\n+b\n@@ -5,1 +5,1 @@\n-' + 'x'.repeat(20) + '\n+y\n');
  assert.equal(mapTokensToRows(rows, () => { throw new Error('engine'); }).size, 0);
  assert.equal(mapTokensToRows(rows, () => null).size, 0);
  assert.equal(mapTokensToRows(rows, () => [[{ text: 'a' }], [{ text: 'b' }]]).size, 0);
  const fake = fakeTokenizer();
  const tokens = mapTokensToRows(rows, fake.tokenize, 10);
  assert.deepEqual(fake.calls, ['a', 'b'], 'the hunk with a long line is skipped entirely');
  assert.deepEqual([...tokens.keys()].sort(), [1, 2]);
});

test('adjacent same-style tokens merge and styles map to inline CSS', () => {
  const rows = parsePatch('@@ -0,0 +1,1 @@\n+ab c\n');
  const tokens = mapTokensToRows(rows, () => [[{ text: 'a', color: 'k' }, { text: '', color: 'z' }, { text: 'b', color: 'k' }, { text: ' c', color: 'f', fontStyle: 3 }]]);
  assert.deepEqual(tokens.get(1), [{ text: 'ab', color: 'k', fontStyle: undefined }, { text: ' c', color: 'f', fontStyle: 3 }]);
  assert.equal(tokenStyle({ text: ' c', color: 'var(--syntax-keyword)', fontStyle: 3 }), 'color:var(--syntax-keyword);font-style:italic;font-weight:600;');
});

test('paths map to Shiki grammars by extension or basename', () => {
  assert.equal(languageFor('cmd/main.go'), 'go');
  assert.equal(languageFor('web/src/App.svelte'), 'svelte');
  assert.equal(languageFor('web/src/a.test.TSX'), 'tsx');
  assert.equal(languageFor('Dockerfile'), 'docker');
  assert.equal(languageFor('deploy/Dockerfile.prod'), 'docker');
  assert.equal(languageFor('Makefile'), 'make');
  assert.equal(languageFor('infra/main.tf'), 'terraform');
  assert.equal(languageFor('LICENSE'), null);
  assert.equal(languageFor('assets/logo.png'), null);
  assert.equal(languageFor('.gitignore'), null);
});

test('every mapped language id names a loadable grammar', async () => {
  for (const id of LANGUAGE_IDS) {
    const tokenize = await loadTokenizer(id);
    assert.equal(tokenize('x\ny').length, 2, id);
  }
});

test('real Shiki colors a Go keyword differently from an identifier, across multi-line comments', async () => {
  const rows = parsePatch('@@ -1,3 +1,3 @@\n /* open\n-old */ func\n+new */ return\n var answer = 42\n');
  const tokens = mapTokensToRows(rows, await loadTokenizer('go'));
  const color = (index: number, text: string) => tokens.get(index)?.find((t) => t.text.trim() === text)?.color;
  assert.equal(color(2, 'old */'), 'var(--syntax-comment)', 'comment continues from the context row on the old side');
  assert.equal(color(3, 'new */'), 'var(--syntax-comment)', 'comment continues from the context row on the new side');
  assert.equal(color(4, 'var'), 'var(--syntax-keyword)');
  assert.notEqual(color(4, 'answer'), color(4, 'var'));
  assert.equal(color(4, '42'), 'var(--syntax-constant)');
  for (const [index, row] of rows.entries()) if (tokens.has(index)) assert.equal(texts(tokens.get(index)), row.text);
});
