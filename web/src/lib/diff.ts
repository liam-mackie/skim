import type { DiffFile, Side } from './api';

/** Opaque path-qualified DOM identity. Use getElementById, never parse it or interpolate it into a CSS selector. */
export function diffElementId(path: string, kind: 'file' | 'line' | 'select' | 'comment', side?: Side, line?: number): string {
  return `diff-${encodeURIComponent(JSON.stringify([path, kind, side ?? null, line ?? null]))}`;
}

export const TEST_ONLY_THRESHOLD = 0.95;

/** Collapse only complete changes positively classified by Jev with two high model scores. */
export function isConfidentTestOnly(file: Pick<DiffFile, 'patchStatus' | 'testOnly'>): boolean {
  const assessment = file.testOnly;
  return file.patchStatus === 'available' && assessment?.onlyTests === true
    && Number.isFinite(assessment.confidence) && assessment.confidence >= TEST_ONLY_THRESHOLD && assessment.confidence <= 1
    && Number.isFinite(assessment.probability) && assessment.probability >= TEST_ONLY_THRESHOLD && assessment.probability <= 1;
}

export type DiffRow =
  | { kind: 'hunk'; text: string }
  | { kind: 'context' | 'add' | 'del'; hunk: number; oldNo: number | null; newNo: number | null; text: string }
  | { kind: 'note'; text: string };

/** Split cells reference canonical parsed rows; null cells have no line or interaction target. */
export type SplitDiffRow =
  | { kind: 'hunk'; index: number }
  | { kind: 'line' | 'note'; left: number | null; right: number | null };

/**
 * Align changed lines by offset within each uninterrupted change block, never across context or hunks.
 * Notes follow their original line on its side, including notes between deletion and addition runs.
 * Only indices are stored: text, line numbers, and hunk identity remain on the original rows.
 */
export function splitDiffRows(rows: readonly DiffRow[]): SplitDiffRow[] {
  const split: SplitDiffRow[] = [];
  const deletions: number[] = [];
  const additions: number[] = [];

  function appendLine(left: number | null, right: number | null) {
    split.push({ kind: 'line', left, right });
    let leftNote = left === null ? rows.length : left + 1;
    let rightNote = right === null ? rows.length : right + 1;
    while (rows[leftNote]?.kind === 'note' || rows[rightNote]?.kind === 'note') {
      const hasLeft = rows[leftNote]?.kind === 'note';
      const hasRight = rows[rightNote]?.kind === 'note';
      split.push({
        kind: 'note',
        left: hasLeft ? leftNote++ : null,
        right: hasRight ? rightNote++ : null,
      });
    }
  }

  let index = 0;
  while (index < rows.length) {
    const row = rows[index]!;
    if (row.kind === 'hunk') {
      split.push({ kind: 'hunk', index: index++ });
    } else if (row.kind === 'context') {
      appendLine(index, index);
      index++;
      while (rows[index]?.kind === 'note') index++;
    } else if (row.kind === 'note') {
      // Keep even an unassociated note rather than discarding patch content.
      split.push({ kind: 'note', left: index, right: index });
      index++;
    } else {
      deletions.length = 0;
      additions.length = 0;
      const hunk = row.hunk;
      while (index < rows.length) {
        const changed = rows[index]!;
        if (changed.kind === 'note') {
          index++;
        } else if ((changed.kind === 'del' || changed.kind === 'add') && changed.hunk === hunk) {
          (changed.kind === 'del' ? deletions : additions).push(index++);
        } else {
          break;
        }
      }
      const count = Math.max(deletions.length, additions.length);
      for (let offset = 0; offset < count; offset++) {
        appendLine(deletions[offset] ?? null, additions[offset] ?? null);
      }
    }
  }
  return split;
}

const HUNK = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

/** Parse a GitHub-style unified patch (hunks only, no file headers) into rows with line numbers. */
export function parsePatch(patch: string): DiffRow[] {
  const rows: DiffRow[] = [];
  if (!patch) return rows;
  let oldNo = 0;
  let newNo = 0;
  let inHunk = false;
  let hunk = -1;
  const lines = patch.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  for (const raw of lines) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    const m = HUNK.exec(line);
    if (m) {
      oldNo = Number(m[1]);
      newNo = Number(m[2]);
      inHunk = true;
      hunk++;
      rows.push({ kind: 'hunk', text: line });
      continue;
    }
    if (!inHunk) continue; // file headers before the first hunk
    const c = line[0];
    if (c === '+') rows.push({ kind: 'add', hunk, oldNo: null, newNo: newNo++, text: line.slice(1) });
    else if (c === '-') rows.push({ kind: 'del', hunk, oldNo: oldNo++, newNo: null, text: line.slice(1) });
    else if (c === '\\') rows.push({ kind: 'note', text: line.slice(1).trim() });
    else rows.push({ kind: 'context', hunk, oldNo: oldNo++, newNo: newNo++, text: line.slice(1) });
  }
  return rows;
}

/** Line number of a row on a given side, or null if the row has none there. */
export function lineOn(row: DiffRow, side: Side): number | null {
  if (row.kind === 'hunk' || row.kind === 'note') return null;
  return side === 'LEFT' ? row.oldNo : row.newNo;
}

/** Natural side of a clicked row: deletions are LEFT, additions and context RIGHT. */
export function sideOf(row: DiffRow): Side | null {
  if (row.kind === 'del') return 'LEFT';
  if (row.kind === 'add' || row.kind === 'context') return 'RIGHT';
  return null;
}
