import assert from 'node:assert/strict';
import test from 'node:test';
import { Lexer } from 'marked';
import type { DiffFile } from '../src/lib/api';
import { appendSuggestion, editSuggestion, findSuggestion, suggestionBody, suggestionSelection } from '../src/lib/suggestion';

const file: DiffFile = {
  path: 'src/code.ts', status: 'modified', additions: 2, deletions: 1, patchStatus: 'available',
  patch: '@@ -1,3 +1,4 @@\n function value() {\n-\treturn old;\n+\tconst result = "<new>";\n+\treturn result;\n }',
};
const anchor = { path: file.path, side: 'RIGHT' as const, startLine: 2, endLine: 3, hunk: 0 };

test('suggestion prefill extracts only the exact new-side range and preserves indentation', () => {
  assert.deepEqual(suggestionSelection(file, anchor), { code: '\tconst result = "<new>";\n\treturn result;', reason: null });
  assert.deepEqual(suggestionSelection(file, { ...anchor, startLine: 1, endLine: 4 }), { code: 'function value() {\n\tconst result = "<new>";\n\treturn result;\n}', reason: null });
  const crlf = { ...file, patch: file.patch.replaceAll('\n', '\r\n') };
  assert.deepEqual(suggestionSelection(crlf, anchor), suggestionSelection(file, anchor));
});

test('missing, stale, old-side, malformed and cross-hunk ranges cannot manufacture a replacement', () => {
  const cases = [
    suggestionSelection(file, { ...anchor, side: 'LEFT' }),
    suggestionSelection(file, anchor, true),
    suggestionSelection(undefined, anchor),
    suggestionSelection({ ...file, patchStatus: 'truncated' }, anchor),
    suggestionSelection(file, { ...anchor, endLine: 5 }),
    suggestionSelection(file, { ...anchor, path: 'different.ts' }),
    suggestionSelection(file, { ...anchor, hunk: 1 }),
    suggestionSelection({ ...file, patch: file.patch.replace('+1,4', '+1,5') }, anchor),
    suggestionSelection({ ...file, patch: file.patch + '\n@@ -10 +11 @@\n later' }, { ...anchor, endLine: 11 }),
  ];
  for (const result of cases) { assert.equal(result.code, null); assert.equal(typeof result.reason, 'string'); }
});

test('fences retain multiline replacements, embedded backticks, trailing blank lines and deletions', () => {
  for (const replacement of ['', '\treturn value;', '\tfirst\n\tsecond', 'const text = `x`;', '```suggestion\nnot a closing fence\n```', 'four ```` ticks', 'line\n', '\n']) {
    const body = suggestionBody(replacement);
    const parsed = findSuggestion(body);
    assert.ok(parsed);
    assert.equal(parsed.code, replacement);
    const blocks = Lexer.lex(body).filter((token) => token.type === 'code');
    assert.equal(blocks.length, 1, 'replacement content cannot escape its suggestion block');
    assert.equal(blocks[0]!.lang, 'suggestion');
  }
});

test('editing a fenced replacement preserves all surrounding review prose and other suggestions', () => {
  const original = appendSuggestion('Explain the consequence.\n', 'old replacement') + '\n\nKeep this footer.\n\n' + suggestionBody('second replacement');
  const first = findSuggestion(original)!;
  const edited = editSuggestion(original, first, 'new ``` replacement\n');
  assert.equal(findSuggestion(edited)?.code, 'new ``` replacement\n');
  assert.ok(edited.startsWith('Explain the consequence.\n\n'));
  assert.ok(edited.endsWith('\n\nKeep this footer.\n\n' + suggestionBody('second replacement')));
  assert.equal(findSuggestion('```suggestion\nnot closed'), null);
  assert.equal(findSuggestion('`suggestion` is ordinary prose.'), null);
});
