import { Lexer, type Tokens } from 'marked';
import type { DiffFile, Side } from './api';
import { parsePatch } from './diff';

export type SuggestionAnchor = { path: string; side: Side; startLine: number; endLine: number; hunk?: number };
export type SuggestionSelection = { code: string; reason: null } | { code: null; reason: string };

const incomplete = 'Code suggestions need the complete selected range in one current diff hunk. Select the lines again from a complete patch.';
const header = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(?:.*)$/;

/** Use the same canonical rows as both diff layouts, never old-side or guessed surrounding code. */
export function suggestionSelection(file: DiffFile | undefined, anchor: SuggestionAnchor, stale = false): SuggestionSelection {
  const unavailable = (reason: string): SuggestionSelection => ({ code: null, reason });
  if (stale) return unavailable('Code suggestions are unavailable for stale code. Select a fresh range on the current pull request head.');
  if (anchor.side !== 'RIGHT') return unavailable('Code suggestions replace new (RIGHT-side) lines. Select new-side code; you can still write an ordinary comment here.');
  if (!file || file.path !== anchor.path || file.patchStatus === 'unavailable' || !file.patch) {
    return unavailable('Code suggestions are unavailable because the current patch is missing. You can still write an ordinary comment.');
  }
  if (file.patchStatus !== 'available') return unavailable('Code suggestions are unavailable for a truncated patch. A complete current patch is required.');
  if (!Number.isSafeInteger(anchor.startLine) || !Number.isSafeInteger(anchor.endLine) || anchor.startLine < 1 || anchor.endLine < anchor.startLine ||
      (anchor.hunk !== undefined && (!Number.isSafeInteger(anchor.hunk) || anchor.hunk < 0))) return unavailable(incomplete);

  // A declared available patch must still account for every line in each hunk.
  // parsePatch intentionally tolerates display-only input; insertion must not guess at malformed rows.
  let inHunk = false;
  for (const raw of file.patch.split('\n')) {
    const line = raw.endsWith('\r') ? raw.slice(0, -1) : raw;
    if (line.startsWith('@@')) {
      if (!header.test(line)) return unavailable(incomplete);
      inHunk = true;
    } else if (inHunk && line !== '' && !/^[ +\-]/.test(line) && line !== '\\ No newline at end of file') {
      return unavailable(incomplete);
    }
  }
  const rows = parsePatch(file.patch);
  let oldCount = 0, newCount = 0, oldExpected = 0, newExpected = 0;
  let currentHunk = -1;
  let selectedHunk: number | undefined;
  let nextLine = anchor.startLine;
  const selected: string[] = [];
  for (const row of rows) {
    if (row.kind === 'hunk') {
      if (oldCount !== oldExpected || newCount !== newExpected) return unavailable(incomplete);
      const match = header.exec(row.text);
      if (!match) return unavailable(incomplete);
      const oldStart = Number(match[1]), newStart = Number(match[3]);
      oldExpected = match[2] === undefined ? 1 : Number(match[2]);
      newExpected = match[4] === undefined ? 1 : Number(match[4]);
      if (![oldStart, newStart, oldExpected, newExpected, oldStart + oldExpected, newStart + newExpected].every(Number.isSafeInteger) ||
          (oldExpected > 0 && oldStart < 1) || (newExpected > 0 && newStart < 1)) return unavailable(incomplete);
      oldCount = 0; newCount = 0; currentHunk++;
    } else if (row.kind !== 'note') {
      if (row.oldNo !== null) oldCount++;
      if (row.newNo !== null) newCount++;
      if (row.newNo !== null && row.newNo >= anchor.startLine && row.newNo <= anchor.endLine) {
        if (row.newNo !== nextLine || (selectedHunk !== undefined && selectedHunk !== currentHunk) ||
            (anchor.hunk !== undefined && anchor.hunk !== currentHunk)) return unavailable(incomplete);
        selectedHunk = currentHunk;
        selected.push(row.text);
        nextLine++;
      }
    }
  }
  if (oldCount !== oldExpected || newCount !== newExpected || nextLine !== anchor.endLine + 1 || !selected.length) return unavailable(incomplete);
  return { code: selected.join('\n'), reason: null };
}

/** The extra newline belongs to the fence, not the replacement. Empty content means deletion. */
export function suggestionBody(replacement: string): string {
  let length = 3;
  for (const run of replacement.matchAll(/`+/g)) length = Math.max(length, run[0].length + 1);
  const fence = '`'.repeat(length);
  return `${fence}suggestion\n${replacement}\n${fence}`;
}

/** Append without trimming or rewriting the user's prose or existing Markdown. */
export function appendSuggestion(body: string, replacement: string): string {
  const separator = !body || body.endsWith('\n\n') ? '' : body.endsWith('\n') ? '\n' : '\n\n';
  return body + separator + suggestionBody(replacement);
}

export type CodeSuggestion = { start: number; end: number; code: string };

/** Locate an ordinary top-level GitHub suggestion for the replacement editor. Markdown stays authoritative. */
export function findSuggestion(body: string): CodeSuggestion | null {
  let offset = 0;
  for (const token of Lexer.lex(body, { gfm: true })) {
    const start = body.indexOf(token.raw, offset);
    if (start < 0) return null; // Do not rewrite bodies normalized by the Markdown lexer.
    offset = start + token.raw.length;
    if (token.type !== 'code' || (token as Tokens.Code).lang !== 'suggestion') continue;
    const opening = /^(`{3,})suggestion[^\S\n]*\n/.exec(token.raw);
    if (!opening) continue;
    const closing = new RegExp(`\n\x60{${opening[1]!.length},}[^\\S\\n]*(?:\\n)?$`).exec(token.raw);
    if (!closing) continue; // Incomplete fences are editable as Markdown, never silently repaired.
    return { start, end: start + closing.index + closing[0].replace(/\n$/, '').length, code: token.raw.slice(opening[0].length, closing.index) };
  }
  return null;
}

export function editSuggestion(body: string, suggestion: CodeSuggestion, replacement: string): string {
  return body.slice(0, suggestion.start) + suggestionBody(replacement) + body.slice(suggestion.end);
}
