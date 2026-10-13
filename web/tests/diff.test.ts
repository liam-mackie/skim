import assert from 'node:assert/strict';
import test from 'node:test';
import { isConfidentTestOnly, lineOn, parsePatch, splitDiffRows, type DiffRow, type SplitDiffRow } from '../src/lib/diff';

/** Every canonical row occurs once on each applicable side, without mutating its content. */
function align(patch: string) {
  const rows = parsePatch(patch);
  for (const row of rows) Object.freeze(row);
  Object.freeze(rows);
  const split = splitDiffRows(rows);
  const left = new Map<number, number>();
  const right = new Map<number, number>();
  const hunks: number[] = [];
  for (const entry of split) {
    if (entry.kind === 'hunk') {
      assert.equal(rows[entry.index]?.kind, 'hunk');
      hunks.push(entry.index);
      continue;
    }
    assert.ok(entry.left !== null || entry.right !== null, 'no invented empty rows');
    for (const [side, counts] of [['left', left], ['right', right]] as const) {
      const index = entry[side];
      if (index === null) continue;
      const original = rows[index];
      assert.ok(original, 'cell references an original row');
      assert.equal(entry.kind === 'note', original.kind === 'note');
      assert.notEqual(original.kind, 'hunk');
      counts.set(index, (counts.get(index) ?? 0) + 1);
    }
  }
  const expectedHunks: number[] = [];
  let preceding: DiffRow | undefined;
  rows.forEach((row, index) => {
    if (row.kind === 'hunk') expectedHunks.push(index);
    const owner = row.kind === 'note' ? preceding : row;
    const bothSides = owner?.kind === 'context' || (row.kind === 'note' && (!owner || owner.kind === 'hunk'));
    assert.equal(left.get(index) ?? 0, Number(bothSides || owner?.kind === 'del'), `left occurrence of row ${index}`);
    assert.equal(right.get(index) ?? 0, Number(bothSides || owner?.kind === 'add'), `right occurrence of row ${index}`);
    if (row.kind !== 'note') preceding = row;
  });
  assert.deepEqual(hunks, expectedHunks);
  return { rows, split };
}

const hunk = (index: number): SplitDiffRow => ({ kind: 'hunk', index });
const line = (left: number | null, right: number | null): SplitDiffRow => ({ kind: 'line', left, right });
const note = (left: number | null, right: number | null): SplitDiffRow => ({ kind: 'note', left, right });

test('unequal replacements pair by position and leave only excess cells empty', () => {
  const longerOld = align('@@ -10,4 +20,3 @@\n before\n-old one\n-old two\n+new one\n after\n');
  assert.deepEqual(longerOld.split, [hunk(0), line(1, 1), line(2, 4), line(3, null), line(5, 5)]);
  assert.equal(lineOn(longerOld.rows[2], 'LEFT'), 11);
  assert.equal(lineOn(longerOld.rows[4], 'RIGHT'), 21);
  assert.equal(longerOld.rows[3].text, 'old two');

  const longerNew = align('@@ -1,1 +1,3 @@\n-old\n+new one\n+new two\n+new three\n');
  assert.deepEqual(longerNew.split, [hunk(0), line(1, 2), line(null, 3), line(null, 4)]);
});

test('add-only and remove-only hunks preserve zero counts and absent-side line numbers', () => {
  const added = align('@@ -0,0 +1,2 @@\n+first\n+second\n');
  assert.deepEqual(added.split, [hunk(0), line(null, 1), line(null, 2)]);
  assert.deepEqual(added.rows.slice(1).map((row) => [lineOn(row, 'LEFT'), lineOn(row, 'RIGHT')]), [[null, 1], [null, 2]]);

  const removed = align('@@ -1,2 +0,0 @@\n-first\n-second\n');
  assert.deepEqual(removed.split, [hunk(0), line(1, null), line(2, null)]);
  assert.deepEqual(removed.rows.slice(1).map((row) => [lineOn(row, 'LEFT'), lineOn(row, 'RIGHT')]), [[1, null], [2, null]]);
});

test('context and hunk boundaries prevent otherwise adjacent changes from pairing', () => {
  const { rows, split } = align('@@ -1,2 +1,2 @@\n-old\n context\n+new\n@@ -10 +20,0 @@\n-removed\n@@ -30,0 +40 @@\n+added\n');
  assert.deepEqual(split, [hunk(0), line(1, null), line(2, 2), line(null, 3), hunk(4), line(5, null), hunk(6), line(null, 7)]);
  assert.deepEqual(rows.filter((row) => 'hunk' in row).map((row) => row.hunk), [0, 0, 0, 1, 2]);
  assert.equal(lineOn(rows[5], 'LEFT'), 10);
  assert.equal(lineOn(rows[7], 'RIGHT'), 40);
});

test('no-newline markers between old and new runs stay with their associated side and line', () => {
  const oldOnly = align('@@ -1 +1 @@\n-old\n\\ No newline at end of file (old)\n+new\n');
  assert.deepEqual(oldOnly.split, [hunk(0), line(1, 3), note(2, null)]);
  assert.equal(oldOnly.rows[2].text, 'No newline at end of file (old)');

  const newOnly = align('@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file (new)\n');
  assert.deepEqual(newOnly.split, [hunk(0), line(1, 2), note(null, 3)]);
  assert.equal(newOnly.rows[3].text, 'No newline at end of file (new)');

  const both = align('@@ -1 +1 @@\n-old\n\\ No newline at end of file (old)\n+new\n\\ No newline at end of file (new)\n');
  assert.deepEqual(both.split, [hunk(0), line(1, 3), note(2, 4)]);
  assert.equal(both.rows[2].text, 'No newline at end of file (old)');
  assert.equal(both.rows[4].text, 'No newline at end of file (new)');
});

test('markers follow their individual paired or excess line, not the end of a replacement block', () => {
  const { split } = align('@@ -1,2 +1,3 @@\n-old one\n-old two\n\\ old marker\n+new one\n+new two\n+new three\n\\ new marker\n');
  assert.deepEqual(split, [hunk(0), line(1, 4), line(2, 5), note(3, null), line(null, 6), note(null, 7)]);
});

test('context markers share their original index on both sides and retain every note', () => {
  const { rows, split } = align('@@ -5 +9 @@\n same text\n\\ No newline at end of file\n\\ additional marker text\n');
  assert.deepEqual(split, [hunk(0), line(1, 1), note(2, 2), note(3, 3)]);
  assert.equal(rows[1].text, 'same text');
  assert.equal(rows[2].text, 'No newline at end of file');
  assert.equal(rows[3].text, 'additional marker text');
  assert.equal(lineOn(rows[2], 'LEFT'), null);
  assert.equal(lineOn(rows[2], 'RIGHT'), null);
});

test('empty patches produce no rows and unassociated notes do not lose content', () => {
  assert.deepEqual(align('').split, []);
  const { rows, split } = align('@@ -0,0 +0,0 @@\n\\ standalone note\n');
  assert.deepEqual(split, [hunk(0), note(1, 1)]);
  assert.equal(rows[1].text, 'standalone note');
});

test('small replacement matrix preserves every line and note exactly once on its original side', () => {
  for (let oldCount = 0; oldCount <= 4; oldCount++) {
    for (let newCount = 0; newCount <= 4; newCount++) {
      for (const oldNote of [false, true]) {
        for (const newNote of [false, true]) {
          const patch = [`@@ -1,${oldCount} +1,${newCount} @@`];
          const oldIndices: number[] = [];
          const newIndices: number[] = [];
          for (let i = 0; i < oldCount; i++) {
            oldIndices.push(patch.length);
            patch.push(`-old ${i}`);
            if (oldNote) patch.push(`\\ old note ${i}`);
          }
          for (let i = 0; i < newCount; i++) {
            newIndices.push(patch.length);
            patch.push(`+new ${i}`);
            if (newNote) patch.push(`\\ new note ${i}`);
          }
          const { split } = align(patch.join('\n'));
          const expected: SplitDiffRow[] = [hunk(0)];
          for (let i = 0; i < Math.max(oldCount, newCount); i++) {
            const left = oldIndices[i] ?? null;
            const right = newIndices[i] ?? null;
            expected.push(line(left, right));
            const leftNote = oldNote && left !== null ? left + 1 : null;
            const rightNote = newNote && right !== null ? right + 1 : null;
            if (leftNote !== null || rightNote !== null) expected.push(note(leftNote, rightNote));
          }
          assert.deepEqual(split, expected, `${oldCount} old / ${newCount} new, notes ${oldNote}/${newNote}`);
        }
      }
    }
  }
});

test('test-only collapse requires a complete positive assessment with both scores at least 95 percent', () => {
  const high = { onlyTests: true, confidence: 0.95, probability: 0.95 };
  assert.equal(isConfidentTestOnly({ patchStatus: 'available', testOnly: high }), true);
  for (const testOnly of [
    undefined,
    { ...high, onlyTests: false },
    { ...high, confidence: 0.949 },
    { ...high, probability: 0.949 },
    { ...high, confidence: NaN },
    { ...high, probability: Infinity },
    { ...high, confidence: 1.1 },
    { ...high, probability: -0.2 },
  ]) assert.equal(isConfidentTestOnly({ patchStatus: 'available', testOnly }), false);
  for (const patchStatus of ['unavailable', 'truncated'] as const) assert.equal(isConfidentTestOnly({ patchStatus, testOnly: high }), false);
});
