<script lang="ts">
  import { untrack } from 'svelte';
  import { store, type Selection } from '../lib/store.svelte';
  import { diffElementId, isConfidentTestOnly, parsePatch, lineOn, splitDiffRows } from '../lib/diff';
  import type { DiffRow, SplitDiffRow } from '../lib/diff';
  import { cachedRowTokens, highlightRows, tokenStyle, type RowTokens, type Token } from '../lib/highlight';
  import type { DiffFile, ReviewComment, ReviewThread, Side } from '../lib/api';
  import { isFlagged, riskSummary } from '../lib/risk';
  import CommentCard from './CommentCard.svelte';
  import InlineComposer from './InlineComposer.svelte';
  import PublishedThread from './PublishedThread.svelte';
  import TreeIcon, { type TreeIconName } from './TreeIcon.svelte';
  import ReviewedCheckbox from './ReviewedCheckbox.svelte';
  import RiskFactors from './RiskFactors.svelte';
  import { Badge } from '$lib/components/ui/badge/index.js';
  import IconButton from '$lib/components/IconButton.svelte';

  let { file }: { file: DiffFile } = $props();
  const bodyId = $props.id();
  let collapseOverride = $state<boolean | null>(null);
  const filePath = $derived(file.path);
  const reviewed = $derived(store.reviewedPaths.has(filePath));
  const collapsed = $derived(collapseOverride ?? (reviewed || (store.collapseTests && isConfidentTestOnly(file))));
  let previousReviewed: boolean | undefined;
  $effect(() => {
    const saved = reviewed;
    if (previousReviewed !== undefined && saved !== previousReviewed) collapseOverride = saved;
    previousReviewed = saved;
  });
  const patch = $derived(file.patch);
  const patchStatus = $derived(file.patchStatus);
  const rows = $derived(patchStatus !== 'unavailable' ? parsePatch(patch) : []);
  const split = $derived(store.diffLayout === 'split');
  const columns = $derived(split ? 4 : 3);
  const sel = $derived(!store.questionStale && store.selection?.path === file.path ? store.selection : null);
  const selectable = $derived(patchStatus === 'available');
  const fileComments = $derived(store.comments.filter((c) => c.path === file.path && (c.status !== 'published' || !store.publishedThreads)));
  const fileThreads = $derived((store.publishedThreads?.threads ?? []).filter((t) => t.path === file.path || (t.outdated && !!file.previousPath && t.path === file.previousPath)));
  const statusIcon = (status: string): TreeIconName =>
    status === 'added' ? 'file-added' : status === 'removed' ? 'file-removed' : status === 'renamed' ? 'file-renamed' : status === 'copied' ? 'file-copied' : status === 'modified' || status === 'changed' ? 'file-modified' : 'file';

  const published = $derived.by(() => {
    const map = new Map<number, ReviewThread[]>();
    const orphans: ReviewThread[] = [];
    for (const thread of fileThreads) {
      if (thread.side !== 'LEFT' && thread.side !== 'RIGHT') {
        orphans.push(thread);
        continue;
      }
      const side = thread.side;
      const start = thread.startLine ?? thread.endLine;
      const first = rows.findIndex((r) => lineOn(r, side) === start);
      const last = rows.findIndex((r) => lineOn(r, side) === thread.endLine);
      const a = rows[first], b = rows[last];
      if (!selectable || thread.outdated || !start || !thread.endLine || first < 0 || last < first || !a || !b || !('hunk' in a) || !('hunk' in b) || a.hunk !== b.hunk) {
        orphans.push(thread);
      } else {
        const existing = map.get(last);
        if (existing) existing.push(thread);
        else map.set(last, [thread]);
      }
    }
    return { map, orphans };
  });

  /** Only current-head comments can be anchored on the displayed diff lines. */
  const current = (c: ReviewComment) => !c.stale && c.headSha === store.pr?.headSha;
  const anchored = $derived.by(() => {
    const map = new Map<number, ReviewComment[]>();
    const placed = new Set<string>();
    rows.forEach((r, i) => {
      for (const c of fileComments) {
        if (placed.has(c.id) || !current(c)) continue;
        if (lineOn(r, c.side) === c.endLine) {
          placed.add(c.id);
          const existing = map.get(i);
          if (existing) existing.push(c);
          else map.set(i, [c]);
        }
      }
    });
    return { map, orphans: fileComments.filter((c) => !placed.has(c.id)) };
  });

  function inSel(r: DiffRow): boolean {
    if (!sel) return false;
    const n = lineOn(r, sel.side);
    return r.kind !== 'hunk' && r.kind !== 'note' && r.hunk === sel.hunk && n !== null && n >= sel.start && n <= sel.end;
  }

  let table = $state<HTMLTableElement>();
  // Highlight lazily: once the table nears the scrolled viewport (observed once) and the file is open.
  // Without IntersectionObserver (e.g. jsdom) rows simply stay plain text.
  let nearViewport = $state(false);
  $effect(() => {
    if (!table || nearViewport || typeof IntersectionObserver === 'undefined') return;
    let root = table.closest('.file-diff')?.parentElement ?? null;
    while (root && !/auto|scroll/.test(getComputedStyle(root).overflowY)) root = root.parentElement;
    const observer = new IntersectionObserver((changes) => {
      if (!changes.some((change) => change.isIntersecting)) return;
      nearViewport = true;
      observer.disconnect();
    }, { root, rootMargin: '800px 0px' });
    observer.observe(table);
    return () => observer.disconnect();
  });
  let highlighted = $state.raw<{ rows: DiffRow[]; tokens: RowTokens | null } | null>(null);
  $effect(() => {
    if (!nearViewport || collapsed || !rows.length) return;
    const target = rows;
    let current = true;
    void highlightRows(filePath, patch, target).then((tokens) => { if (current) highlighted = { rows: target, tokens }; });
    return () => { current = false; };
  });
  // Results for an older patch are ignored; remounts reuse the module cache without a plain-text flash.
  const rowTokens = $derived(highlighted?.rows === rows ? highlighted.tokens : rows.length ? cachedRowTokens(filePath, patch) ?? null : null);
  let gesture: { pointerId: number; anchor: Selection; baseSha: string; headSha: string; table: HTMLTableElement } | null = null;

  function finishGesture(focus: boolean) {
    const g = gesture;
    if (!g) return;
    gesture = null;
    g.table.removeEventListener('pointermove', moveGesture);
    g.table.removeEventListener('pointerup', endGesture);
    g.table.removeEventListener('pointercancel', cancelGesture);
    g.table.removeEventListener('lostpointercapture', cancelGesture);
    if (g.table.hasPointerCapture(g.pointerId)) g.table.releasePointerCapture(g.pointerId);
    if (focus) store.questionFocus++;
  }

  function moveGesture(e: PointerEvent) {
    const g = gesture;
    if (!g || e.pointerId !== g.pointerId) return;
    if (file.path !== g.anchor.path || store.pr?.baseSha !== g.baseSha || store.pr?.headSha !== g.headSha || !selectable) {
      finishGesture(false);
      return;
    }
    const hit = document.elementFromPoint(e.clientX, e.clientY);
    const cell = hit?.closest<HTMLElement>('[data-row]');
    if (!cell || !g.table.contains(cell)) return;
    const side = hit?.closest<HTMLElement>('td[data-side]')?.dataset.side;
    if (side && side !== g.anchor.side) return;
    const r = rows[Number(cell.dataset.row)];
    if (!r || r.kind === 'hunk' || r.kind === 'note' || r.hunk !== g.anchor.hunk) return;
    const line = lineOn(r, g.anchor.side);
    if (line === null) return;
    store.selectQuestion({ ...g.anchor, start: Math.min(g.anchor.start, line), end: Math.max(g.anchor.end, line) }, diffElementId(file.path, 'select', g.anchor.side, line));
  }

  function endGesture(e: PointerEvent) {
    if (e.pointerId !== gesture?.pointerId) return;
    moveGesture(e);
    finishGesture(true);
  }

  function cancelGesture(e: PointerEvent) {
    if (e.pointerId === gesture?.pointerId) finishGesture(false);
  }

  function startGesture(e: PointerEvent, r: DiffRow, side: Side) {
    if (e.button !== 0 || !e.isPrimary || !store.pr || !table || !selectable || r.kind === 'hunk' || r.kind === 'note') return;
    const line = lineOn(r, side);
    if (line === null) return;
    e.preventDefault();
    finishGesture(false);
    if (!store.clickLine(file.path, side, line, r.hunk, e.shiftKey) || !store.selection) return;
    collapseOverride = false;
    gesture = { pointerId: e.pointerId, anchor: { ...store.selection }, baseSha: store.pr.baseSha, headSha: store.pr.headSha, table };
    table.addEventListener('pointermove', moveGesture);
    table.addEventListener('pointerup', endGesture);
    table.addEventListener('pointercancel', cancelGesture);
    table.addEventListener('lostpointercapture', cancelGesture);
    table.setPointerCapture(e.pointerId);
  }

  function activateLine(e: MouseEvent, r: DiffRow, side: Side) {
    // Pointer selection finishes on pointerup; its synthetic click must not collapse a drag.
    if (e.detail !== 0 || !selectable || r.kind === 'hunk' || r.kind === 'note') return;
    const line = lineOn(r, side);
    if (line !== null && store.clickLine(file.path, side, line, r.hunk, e.shiftKey)) {
      collapseOverride = false;
      store.questionFocus++;
    }
  }

  function comment(r: DiffRow, side: Side) {
    if (!selectable || r.kind === 'hunk' || r.kind === 'note') return;
    const n = lineOn(r, side);
    if (n === null) return;
    const anchor: Selection = sel?.side === side && sel.hunk === r.hunk && n >= sel.start && n <= sel.end
      ? { ...sel } : { path: file.path, side, start: n, end: n, hunk: r.hunk };
    store.openComposer(anchor, diffElementId(file.path, 'comment', side, n));
  }

  $effect(() => {
    void filePath;
    void store.pr?.baseSha;
    void store.pr?.headSha;
    void selectable;
    void split;
    return () => finishGesture(false);
  });

  const ownsComposer = $derived(!!store.composer && !store.composerStale && store.composer.anchor.path === file.path);
  const composerRow = $derived.by(() => {
    const c = store.composer;
    if (!ownsComposer || !c || !selectable) return -1;
    return rows.findIndex((r) => r.kind !== 'hunk' && r.kind !== 'note' && r.hunk === c.anchor.hunk && lineOn(r, c.anchor.side) === c.anchor.end);
  });

  type Entry =
    | { kind: 'code'; key: string; row: SplitDiffRow }
    | { kind: 'composer'; key: string }
    | { kind: 'comment'; key: string; comment: ReviewComment }
    | { kind: 'thread'; key: string; thread: ReviewThread };
  // Keep interactive children in one keyed list: pairing code must not remount drafts.
  const entries = $derived.by(() => {
    const display: SplitDiffRow[] = split ? splitDiffRows(rows) : rows.map((r, index) =>
      r.kind === 'hunk' ? { kind: 'hunk', index } : { kind: r.kind === 'note' ? 'note' : 'line', left: index, right: null });
    const result: Entry[] = [];
    for (const row of display) {
      result.push({ kind: 'code', key: row.kind === 'hunk' ? `hunk:${row.index}` : `code:${row.left}:${row.right}`, row });
      if (row.kind !== 'line') continue;
      const indices = row.left === row.right ? [row.left] : [row.left, row.right];
      for (const index of indices) {
        if (index === null) continue;
        if (composerRow === index) result.push({ kind: 'composer', key: 'composer' });
        for (const comment of anchored.map.get(index) ?? []) result.push({ kind: 'comment', key: `comment:${comment.id}`, comment });
        for (const thread of published.map.get(index) ?? []) result.push({ kind: 'thread', key: `thread:${thread.id}`, thread });
      }
    }
    return result;
  });

  $effect(() => {
    const navigation = store.fileNavigation;
    if (navigation?.path === filePath && navigation.unitId === undefined && (navigation.expand !== false || navigation.line)) {
      void navigation.nonce;
      collapseOverride = false;
    }
  });

  $effect(() => {
    void store.composerFocus;
    if (ownsComposer) collapseOverride = false;
  });

  let previousCollapseTests: boolean | undefined;
  $effect.pre(() => {
    const enabled = store.collapseTests;
    if (enabled && previousCollapseTests === false) {
      // Only reapply on an explicit off→on transition, not on editor or selection updates.
      untrack(() => {
        if (isConfidentTestOnly(file) && collapseOverride !== true) {
          collapseOverride = sel || ownsComposer || document.getElementById(bodyId)?.querySelector('form') ? false : null;
        }
      });
    }
    previousCollapseTests = enabled;
  });
</script>

{#snippet gutter(r: DiffRow, side: Side, n: number | null)}
  {#if n !== null}
    <div class="gutter">
      <button class="line" id={diffElementId(file.path, 'select', side, n)} disabled={!selectable || !!store.busy.verify} aria-describedby={!selectable || !!store.busy.verify ? `${bodyId}-selection-status` : undefined} onpointerdown={(e) => startGesture(e, r, side)} onclick={(e) => activateLine(e, r, side)} aria-label="Select {side === 'LEFT' ? 'old' : 'new'} line {n} for AI" aria-pressed={!!sel && sel.side === side && inSel(r)}>{n}</button>
      {#if selectable}
        <button class="plus" id={diffElementId(file.path, 'comment', side, n)} onclick={() => comment(r, side)} aria-label="Comment on {side === 'LEFT' ? 'old' : 'new'} line {n}" title="Add a pending comment">+</button>
      {/if}
    </div>
  {/if}
{/snippet}

{#snippet codeText(tokens: Token[] | undefined, text: string)}{#if tokens}{#each tokens as token}<span style={tokenStyle(token)}>{token.text}</span>{/each}{:else}{text}{/if}{/snippet}

{#snippet splitCells(index: number | null, side: Side)}
  {@const r = index === null ? null : rows[index]}
  {@const n = r ? lineOn(r, side) : null}
  {@const selected = !!r && sel?.side === side && inSel(r)}
  <td class="num {r?.kind ?? 'empty'}" class:sel={selected} class:divider={side === 'RIGHT'} data-side={side} data-row={index ?? undefined} id={n !== null ? diffElementId(file.path, 'line', side, n) : undefined}>
    {#if r}{@render gutter(r, side, n)}{/if}
  </td>
  <td class="code mono {r?.kind ?? 'empty'}" class:sel={selected} data-side={side} data-row={index ?? undefined}>
    {#if r}<span class="sign">{r.kind === 'add' ? '+' : r.kind === 'del' ? '-' : ' '}</span>{@render codeText(index === null ? undefined : rowTokens?.get(index), r.text)}{/if}
  </td>
{/snippet}

<!-- All navigation and return-focus IDs share the path-qualified diffElementId contract. -->
<article class="file-diff" id={diffElementId(file.path, 'file')} data-file-path={file.path}>
  <header class="filehead" class:collapsed>
    <IconButton size="icon-sm" class="[@media(pointer:coarse)]:min-h-8" onclick={() => { finishGesture(false); collapseOverride = !collapsed; }} aria-expanded={!collapsed} aria-controls={bodyId} label={`${collapsed ? 'Expand' : 'Collapse'} ${file.path}`} tooltip={collapsed ? 'Expand file' : 'Collapse file'}>
      <span class:closed={collapsed}><TreeIcon name="chevron" /></span>
    </IconButton>
    <span class="status" class:added={file.status === 'added'} class:removed={file.status === 'removed'} title={file.status} aria-label={file.status}><TreeIcon name={statusIcon(file.status)} /></span>
    <h2 class="mono">{#if file.previousPath && file.previousPath !== file.path}<span class="previous">{file.previousPath} → </span>{/if}{file.path}</h2>
    <span class="stats"><span class="add">+{file.additions}</span><span class="del">−{file.deletions}</span></span>
    {#if isFlagged(file.risk)}<Badge variant="outline" class={['shrink-0 text-[10px]', file.risk.level === 'high' ? 'text-[var(--risk-high)]' : 'text-[var(--risk-medium)]']} title={riskSummary(file.risk)} aria-label={riskSummary(file.risk)}>{file.risk.level} risk</Badge>{/if}
    <ReviewedCheckbox paths={[file.path]} ariaLabel={`Reviewed ${file.path}`} />
  </header>

  <!-- Keep children mounted while collapsed so comment edits and drafts retain their text. -->
  <div class="filebody" id={bodyId} hidden={collapsed}>
    {#if isFlagged(file.risk)}<RiskFactors path={file.path} risk={file.risk} />{/if}
    <span class="sr-only" id={`${bodyId}-selection-status`}>{!selectable ? 'Line selection is unavailable because this patch is incomplete.' : store.busy.verify ? 'Line selection is unavailable while verification is running.' : ''}</span>
    {#if file.patchStatus === 'unavailable'}
      <div class="warn">
        No patch is available for this file (binary content, or GitHub omitted the diff). It can’t be line-selected; use the global chat or open it on GitHub.
      </div>
    {:else if file.patchStatus === 'truncated'}
      <div class="warn">
        This patch is truncated: it does not cover all {file.additions + file.deletions} reported changed lines. It is shown for reading only; line selection, comments and verification are disabled. Use the global chat for this file.
      </div>
    {/if}
    {#if file.patchStatus !== 'unavailable' && rows.length === 0}
      <p class="muted pad">The patch contains no hunks to display.</p>
    {/if}

    {#if rows.length}
      <div class="diff-horizontal" role="region" aria-label="Scrollable diff of {file.path}">
        <table class="diff" class:split bind:this={table} aria-label="Diff of {file.path}">
          <colgroup>
            <col style="width: 56px" />
            {#if split}<col />{/if}
            <col style="width: 56px" /><col />
          </colgroup>
          {#if split}
            <thead><tr><th colspan="2" scope="colgroup">Old</th><th colspan="2" scope="colgroup">New</th></tr></thead>
          {/if}
          <tbody>
            {#each entries as entry (entry.key)}
              {#if entry.kind === 'composer'}
                <tr class="cmt"><td colspan={columns}><div class="cmtwrap"><InlineComposer /></div></td></tr>
              {:else if entry.kind === 'comment'}
                <tr class="cmt"><td colspan={columns}><div class="cmtwrap"><CommentCard c={entry.comment} /></div></td></tr>
              {:else if entry.kind === 'thread'}
                <tr class="cmt"><td colspan={columns}><div class="cmtwrap"><PublishedThread thread={entry.thread} /></div></td></tr>
              {:else if entry.row.kind === 'hunk'}
                <tr class="hunk"><td colspan={columns} class="mono">{rows[entry.row.index]?.text}</td></tr>
              {:else if split}
                {#if entry.row.kind === 'note'}
                  <tr class="note">
                    <td colspan="2" class="mono">{entry.row.left !== null ? rows[entry.row.left]?.text : ''}</td>
                    <td colspan="2" class="mono divider">{entry.row.right !== null ? rows[entry.row.right]?.text : ''}</td>
                  </tr>
                {:else}
                  <tr>
                    {@render splitCells(entry.row.left, 'LEFT')}
                    {@render splitCells(entry.row.right, 'RIGHT')}
                  </tr>
                {/if}
              {:else}
                {@const index = entry.row.left!}
                {@const r = rows[index]!}
                {#if r.kind === 'note'}
                  <tr class="note"><td colspan={columns} class="mono">{r.text}</td></tr>
                {:else if r.kind !== 'hunk'}
                  <tr class="{r.kind}" class:sel={inSel(r)} data-row={index}>
                    <td class="num" data-side="LEFT" id={r.oldNo !== null ? diffElementId(file.path, 'line', 'LEFT', r.oldNo) : undefined}>
                      {@render gutter(r, 'LEFT', r.oldNo)}
                    </td>
                    <td class="num" data-side="RIGHT" id={r.newNo !== null ? diffElementId(file.path, 'line', 'RIGHT', r.newNo) : undefined}>
                      {@render gutter(r, 'RIGHT', r.newNo)}
                    </td>
                    <td class="code mono"><span class="sign">{r.kind === 'add' ? '+' : r.kind === 'del' ? '-' : ' '}</span>{@render codeText(rowTokens?.get(index), r.text)}</td>
                  </tr>
                {/if}
              {/if}
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
    {#if anchored.orphans.length || published.orphans.length}
      <section class="pad">
        <h3>Outdated comments and comments outside the displayed diff</h3>
        <p class="hint">Comments from an earlier PR head are never shown against the current code.</p>
        {#each anchored.orphans as c (c.id)}<CommentCard {c} showLocation />{/each}
        {#each published.orphans as thread (thread.id)}<PublishedThread {thread} showLocation />{/each}
      </section>
    {/if}
    {#if ownsComposer && composerRow === -1}<InlineComposer />{/if}
  </div>
</article>

<style>
  .file-diff { min-width: 0; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg); container-type: inline-size; }
  .filehead { position: sticky; top: 0; z-index: 2; display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 6px 10px; min-height: 40px; border-bottom: 1px solid var(--border); border-radius: var(--radius) var(--radius) 0 0; background: var(--bg-subtle); }
  .filehead.collapsed { border-bottom: 0; border-radius: var(--radius); }
  .closed { transform: rotate(-90deg); }
  .status { flex: 0 0 auto; color: var(--fg-muted); }
  .status.added { color: var(--add-fg); }
  .status.removed { color: var(--del-fg); }
  h2 { flex: 1; min-width: 0; margin: 0; font-size: 12px; font-weight: 500; overflow-wrap: anywhere; }
  .previous { color: var(--fg-muted); }
  .stats { display: flex; flex: 0 0 auto; gap: 6px; font-size: 11px; font-variant-numeric: tabular-nums; }
  .add { color: var(--add-fg); }
  .del { color: var(--del-fg); }
  .filebody[hidden] { display: none; }
  .pad { padding: 0 16px; }
  .warn { margin: 8px 16px; }
  .diff-horizontal { overflow-x: auto; }
  table.diff { width: max-content; min-width: 100%; border-collapse: collapse; font-size: 12px; }
  table.diff.split { width: 100%; min-width: 640px; table-layout: fixed; }
  .split th { padding: 5px 12px; text-align: left; font-size: 11px; font-weight: 500; color: var(--fg-muted); background: var(--bg-subtle); border-bottom: 1px solid var(--border); }
  .split th + th, .divider { border-left: 1px solid var(--border); }
  .split td.code { white-space: pre-wrap; overflow-wrap: anywhere; }
  .split td.add { background: var(--add-bg); }
  .split td.num.add { background: var(--add-num); }
  .split td.del { background: var(--del-bg); }
  .split td.num.del { background: var(--del-num); }
  .split td.empty { background: var(--bg-inset); }
  .split tr.note td { padding: 0 10px; white-space: pre-wrap; overflow-wrap: anywhere; }
  td { padding: 0; vertical-align: top; }
  td.num { width: 56px; min-width: 56px; text-align: right; user-select: none; color: var(--fg-muted); }
  .gutter { display: flex; align-items: center; min-height: 24px; }
  td.num button { border: 0; border-radius: 3px; background: transparent; padding: 0; min-height: 24px; cursor: pointer; font-family: var(--mono); font-size: 11px; line-height: 24px; color: inherit; }
  td.num .line { flex: 1; text-align: right; padding-right: 5px; touch-action: none; user-select: none; }
  td.num .line[aria-pressed='true'] { color: var(--fg); font-weight: 600; border-radius: 0; }
  td.num .plus { flex: 0 0 22px; color: var(--accent); font-size: 18px; opacity: 0; }
  tr:hover .plus, .gutter:focus-within .plus, td.num .plus:focus-visible { opacity: 1; background: var(--accent-solid); color: #fff; }
  td.num button:disabled { cursor: default; }
  td.num button:hover:not(:disabled) { text-decoration: underline; }
  td.num button:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  td.code { white-space: pre; padding: 0 10px; line-height: 24px; }
  .sign { display: inline-block; width: 1.2ch; color: var(--fg-muted); user-select: none; }
  tr.add { background: var(--add-bg); }
  tr.add td.num { background: var(--add-num); }
  tr.del { background: var(--del-bg); }
  tr.del td.num { background: var(--del-num); }
  tr.hunk td { background: var(--hunk-bg); color: var(--fg-muted); padding: 5px 12px; font-size: 11px; white-space: pre; }
  tr.note td { color: var(--fg-muted); font-style: italic; padding: 0 8px 0 120px; white-space: pre; }
  tr.sel td, td.sel { background: var(--sel-bg) !important; }
  tr.cmt td { background: var(--bg-subtle); }
  .cmtwrap { position: sticky; left: 0; width: 100cqw; box-sizing: border-box; padding: 4px 16px; white-space: normal; }
  h3 { font-size: 13px; }
  @media (hover: none), (pointer: coarse) {
    td.num .plus { opacity: 1; min-height: 32px; }
    td.num .line { min-height: 32px; }
  }
  @media (max-width: 800px) {
    .filehead { gap: 6px; padding: 6px; flex-wrap: wrap; }
    h2 { flex-basis: calc(100% - 64px); }
    .stats { margin-left: 56px; }
    .cmtwrap { padding: 4px 8px; }
  }
</style>
