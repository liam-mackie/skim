<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { store } from '../lib/store.svelte';
  import type { DiffFile } from '../lib/api';
  import { diffElementId } from '../lib/diff';
  import { snapshotKey } from '../lib/concurrency';
  import { buildTree, unitElementId, validatedUnits, type TreeNode, type Unit } from '../lib/tree';
  import FileDiff from './FileDiff.svelte';
  import InlineComposer from './InlineComposer.svelte';
  import QuestionPanel from './QuestionPanel.svelte';
  import PRContext from './PRContext.svelte';
  import UnitContext from './UnitContext.svelte';
  import GuideOverview from './GuideOverview.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';

  let { guide = false }: { guide?: boolean } = $props();
  let scrollContainer = $state<HTMLDivElement>();
  let readerWidth = $state(0);
  let viewportHeight = $state(0);
  const snapshot = $derived(snapshotKey(store.pr));
  const visible = $derived(store.view === 'pr' && (store.prTab === 'diff' || store.prTab === 'guide') && !store.settingsOpen);
  const units = $derived(validatedUnits(store.state?.groups ?? [], store.files));
  const recommended = $derived(guide && !!units);
  const guideWide = $derived(recommended && readerWidth >= 780);
  const guidePreparing = $derived(guide && !recommended && (store.analysisRefreshing || store.state?.analysis?.grouping.status === 'running'));
  let unitOverrides = $state<Record<string, { reviewed: boolean; collapsed: boolean }>>({});
  const unitStates = $derived((units ?? []).map((unit) => {
    const key = JSON.stringify([snapshot, unit.id, unit.files.map((file) => file.path)]);
    const reviewed = unit.files.every((file) => store.reviewedPaths.has(file.path));
    const override = unitOverrides[key];
    return { unit, key, reviewed, collapsed: override?.reviewed === reviewed ? override.collapsed : reviewed };
  }));
  // An override belongs to this membership and completion state, not a reusable unit ID.
  $effect.pre(() => {
    const states = unitStates.map(({ key, reviewed }) => ({ key, reviewed }));
    untrack(() => {
      for (const key of Object.keys(unitOverrides)) {
        if (!states.some((state) => state.key === key && state.reviewed === unitOverrides[key]!.reviewed)) delete unitOverrides[key];
      }
    });
  });
  function setUnitCollapsed(unitId: string, collapsed: boolean) {
    const state = unitStates.find(({ unit }) => unit.id === unitId);
    if (state) unitOverrides[state.key] = { reviewed: state.reviewed, collapsed };
  }
  const entries = $derived.by(() => {
    const result: { file: DiffFile; row: number; unit?: Unit; position?: number; unitId?: string; collapsed?: boolean; span?: number }[] = [];
    if (recommended) {
      let row = 1;
      unitStates.forEach(({ unit, collapsed }, index) => {
        unit.files.forEach((file, fileIndex) => result.push({
          file, row: collapsed ? row : row + fileIndex, unitId: unit.id, collapsed,
          ...(fileIndex === 0 ? { unit, position: index + 1, span: collapsed ? 1 : unit.files.length } : {}),
        }));
        row += collapsed ? 1 : unit.files.length;
      });
    } else {
      const visit = (nodes: TreeNode[]) => {
        for (const node of nodes) {
          if (node.kind === 'dir') visit(node.children);
          else result.push({ file: node.file, row: result.length + 1 });
        }
      };
      visit(buildTree(store.files));
    }
    return result;
  });
  // Badge polls do not rebuild scroll tracking or reset the reading position.
  const structure = $derived(JSON.stringify(entries.map(({ file, unit, collapsed }) => [file.path, unit?.id, unit?.title, unit?.summary, collapsed])));
  const orphanComposer = $derived(!!store.composer && (store.composerStale || !store.files.some((file) => file.path === store.composer?.anchor.path)));
  let visibleCards = new Set<HTMLElement>();
  let frame = 0;
  let adjusting = false;
  let handledNavigation: typeof store.fileNavigation = null;
  let navigationPosition: number | null = null;
  let previousSnapshot = '';
  let previousStructure = '';
  let previousLayout = '';
  let previousCollapsed = new Set<string>();

  function trackVisibleFile() {
    if (frame) return;
    frame = requestAnimationFrame(() => {
      frame = 0;
      const scroller = scrollContainer;
      if (!visible || adjusting || !scroller?.clientHeight || !scroller.clientWidth) return;
      // At the end of a PR, a short target cannot reach the top. Keep the clicked
      // file active until the reader actually moves, rather than selecting its predecessor.
      if (navigationPosition !== null && Math.abs(scroller.scrollTop - navigationPosition) < 1) return;
      navigationPosition = null;
      const readingLine = scroller.getBoundingClientRect().top + 40;
      let current: HTMLElement | null = null;
      let currentTop = -Infinity;
      let upcoming: HTMLElement | null = null;
      let upcomingTop = Infinity;
      for (const card of visibleCards) {
        const rect = card.getBoundingClientRect();
        if (!rect.height || !rect.width || (card.dataset.unitPath && !card.classList.contains('collapsed'))) continue;
        if (rect.bottom <= readingLine) continue;
        if (rect.top <= readingLine && rect.top > currentTop) { current = card; currentTop = rect.top; }
        else if (rect.top > readingLine && rect.top < upcomingTop) { upcoming = card; upcomingTop = rect.top; }
      }
      const active = current ?? upcoming;
      const path = active?.dataset.filePath ?? active?.dataset.unitPath;
      // Observing scroll never issues a jump or moves a comment/question anchor.
      if (path && store.selectedPath !== path) store.selectedPath = path;
    });
  }

  async function applyNavigation() {
    const request = store.fileNavigation;
    const identity = snapshot;
    if (!request || request === handledNavigation || !visible) return;
    if (request.unitId !== undefined && (!recommended || request.snapshot !== snapshot || request.generation !== store.state?.analysis?.generation)) return;
    if (recommended) {
      const target = unitStates.find(({ unit }) => request.unitId !== undefined ? unit.id === request.unitId : unit.files.some((file) => file.path === request.path));
      if (target?.collapsed) setUnitCollapsed(target.unit.id, false);
    }
    await tick();
    const scroller = scrollContainer;
    if (request === handledNavigation || request !== store.fileNavigation || identity !== snapshot || !scroller?.clientHeight || !scroller.clientWidth) return;
    if (request.unitId !== undefined) {
      if (!recommended || request.snapshot !== snapshot || request.generation !== store.state?.analysis?.generation) return;
      const explanation = document.getElementById(unitElementId(request.unitId));
      if (!explanation || !scroller.contains(explanation)) return;
      adjusting = true;
      handledNavigation = request;
      scroller.scrollTop += explanation.getBoundingClientRect().top - scroller.getBoundingClientRect().top - 16;
      navigationPosition = scroller.scrollTop;
      explanation.querySelector('h2')?.focus({ preventScroll: true });
      requestAnimationFrame(() => { adjusting = false; trackVisibleFile(); });
      return;
    }
    const card = document.getElementById(diffElementId(request.path, 'file'));
    if (!card || !scroller.contains(card)) return;
    const line = request.line && document.getElementById(diffElementId(request.path, 'line', request.line.side, request.line.line));
    const target = line && card.contains(line) ? line : card;
    const inset = target === card ? 12 : (card.querySelector('.filehead')?.getBoundingClientRect().height ?? 44) + 12;
    adjusting = true;
    handledNavigation = request;
    scroller.scrollTop += target.getBoundingClientRect().top - scroller.getBoundingClientRect().top - inset;
    navigationPosition = scroller.scrollTop;
    requestAnimationFrame(() => { adjusting = false; trackVisibleFile(); });
  }

  $effect(() => {
    void store.fileNavigation;
    void visible;
    void store.mobileView;
    void structure;
    untrack(() => { void applyNavigation(); });
  });

  $effect(() => {
    void store.composerFocus;
    const composer = store.composer;
    untrack(() => {
      if (!composer || store.composerStale) return;
      const target = unitStates.find(({ unit }) => unit.files.some((file) => file.path === composer.anchor.path));
      if (target?.collapsed) setUnitCollapsed(target.unit.id, false);
    });
  });

  // Reordering preserves the visible file; layout changes preserve its visible code line.
  $effect.pre(() => {
    const nextSnapshot = snapshot;
    const nextStructure = structure;
    const nextLayout = `${store.diffLayout}:${guideWide}`;
    const layoutChanged = nextLayout !== previousLayout;
    if (nextSnapshot === previousSnapshot && nextStructure === previousStructure && !layoutChanged) return;
    const sameSnapshot = nextSnapshot === previousSnapshot;
    previousSnapshot = nextSnapshot;
    previousStructure = nextStructure;
    previousLayout = nextLayout;
    const collapsedIds = new Set(entries.filter((entry) => entry.unit && entry.collapsed).map((entry) => entry.unit!.id));
    const newlyCollapsed = entries.filter((entry) => entry.unit && entry.collapsed && !previousCollapsed.has(entry.unit.id));
    previousCollapsed = collapsedIds;
    untrack(() => {
      const scroller = scrollContainer;
      if (!scroller) return;
      if (!sameSnapshot) {
        handledNavigation = null;
        navigationPosition = null;
        visibleCards.clear();
        scroller.scrollTop = 0;
        return;
      }
      if (adjusting) return;
      const active = document.activeElement;
      const activePath = active?.closest<HTMLElement>('[data-file-path]')?.dataset.filePath;
      const path = activePath ?? store.selectedPath;
      const entry = entries.find(({ file }) => file.path === path);
      const card = path ? document.getElementById(diffElementId(path, 'file')) : null;
      const bounds = scroller.getBoundingClientRect();
      const collapsing = newlyCollapsed.find(({ unit }) => unit!.files.some((file) => {
        const rect = document.getElementById(diffElementId(file.path, 'file'))?.getBoundingClientRect();
        return rect && rect.height && rect.bottom > bounds.top && rect.top < bounds.bottom;
      }));
      const unitId = collapsing?.unit?.id ?? (entry?.collapsed ? entry.unitId : undefined);
      const collapsedHeader = unitId ? document.getElementById(unitElementId(unitId)) : null;
      let anchor = collapsedHeader ?? card;
      if (!anchor || !scroller.contains(anchor) || !anchor.getBoundingClientRect().height) return;
      if (layoutChanged && !collapsedHeader && card) {
        const bounds = scroller.getBoundingClientRect();
        const firstLine = bounds.top + (card.querySelector('.filehead')?.getBoundingClientRect().height ?? 40);
        for (const cell of card.querySelectorAll<HTMLElement>('td.num[id]')) {
          const rect = cell.getBoundingClientRect();
          if (rect.height && rect.top >= firstLine && rect.top < bounds.bottom) { anchor = cell; break; }
        }
      }
      const anchorId = anchor.id;
      const offset = collapsedHeader
        ? Math.max(16, anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top)
        : anchor.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
      const request = store.fileNavigation;
      adjusting = true;
      void tick().then(() => {
        const target = document.getElementById(anchorId);
        if (nextSnapshot === snapshot && request === store.fileNavigation && target?.getBoundingClientRect().height && scroller.contains(target)) {
          scroller.scrollTop += target.getBoundingClientRect().top - scroller.getBoundingClientRect().top - offset;
          if (navigationPosition !== null) navigationPosition = scroller.scrollTop;
        }
        adjusting = false;
        trackVisibleFile();
      });
    });
  });

  $effect(() => {
    const scroller = scrollContainer;
    void structure;
    void snapshot;
    if (!scroller) return;
    let disposed = false;
    const observer = new IntersectionObserver((changes) => {
      for (const change of changes) {
        if (change.isIntersecting) visibleCards.add(change.target as HTMLElement);
        else visibleCards.delete(change.target as HTMLElement);
      }
      trackVisibleFile();
    }, { root: scroller });
    const resize = new ResizeObserver(() => { void applyNavigation(); trackVisibleFile(); });
    resize.observe(scroller);
    void tick().then(() => {
      if (disposed) return;
      visibleCards.clear();
      for (const card of scroller.querySelectorAll<HTMLElement>('[data-file-path], [data-unit-path]')) observer.observe(card);
      void applyNavigation();
    });
    return () => { disposed = true; observer.disconnect(); resize.disconnect(); visibleCards.clear(); };
  });
  $effect(() => () => { if (frame) cancelAnimationFrame(frame); });
</script>

<div class="diff-viewport">
  <div class="diff-content" bind:this={scrollContainer} bind:clientWidth={readerWidth} bind:clientHeight={viewportHeight} onscroll={trackVisibleFile}>
    {#if !store.pr}
      <div class="empty"><h2>No pull request loaded</h2></div>
    {:else}
      <PRContext {scrollContainer} {guide} />
      <div class="flex items-center justify-end gap-1 px-4 pt-2">
        <Button variant="ghost" size="sm" onclick={() => { store.tab = 'comments'; store.mobileView = 'panel'; }} aria-label={store.publishedThreads ? `${store.publishedThreads.threads.length} GitHub discussions` : 'GitHub discussions'}>
          <MessagesSquare aria-hidden="true" />GitHub{#if store.publishedThreads}<span class="tabular-nums">{store.publishedThreads.threads.length}</span>{/if}
        </Button>
        <IconButton label="Refresh comments" text="Refresh" tooltip={store.publishedThreadsLoading ? 'Refreshing comments…' : 'Refresh comments'} size="sm" onclick={() => store.loadPublishedThreads(true)} disabled={store.publishedThreadsLoading}><RefreshCw class={store.publishedThreadsLoading ? 'animate-spin' : ''} /></IconButton>
      </div>
      {#if store.reviewError}<div class="error discussions-error" role="alert">Review progress could not be saved: {store.reviewError}</div>{/if}
      {#if store.publishedThreadsError}
        <div class="error discussions-error" role="alert">
          <p>Existing comments could not be refreshed: {store.publishedThreadsError}{#if store.publishedThreads} Showing the last loaded discussions.{/if}</p>
          <Button variant="outline" size="sm" onclick={() => store.pr && store.openPullRequest(store.pr.url, store.prTab)} disabled={!!store.blockReason('load')} title={store.blockReason('load') ?? 'Reload this pull request and refresh its GitHub discussions'}><RefreshCw class={store.busy.load ? 'animate-spin' : ''} aria-hidden="true" />{store.busy.load ? 'Reloading…' : 'Reload pull request'}</Button>
          {#if store.blockReason('load')}<p class="hint">{store.blockReason('load')}</p>{/if}
        </div>
      {/if}
      {#if orphanComposer}<InlineComposer />{/if}
      {#if !entries.length}
        <div class="empty"><p class="muted">This pull request has no changed files.</p></div>
      {:else}
        {#if guide && !recommended}<GuideOverview inGuide />{/if}
        {#key snapshot}
          <!-- Hidden, not unmounted, while the guide prepares: open editors and drafts survive. -->
          <div class="stream" class:guide={guideWide} hidden={guidePreparing}>
            {#each entries as entry (entry.file.path)}
              {#if entry.unit}
                <section class="unit-context" class:collapsed={entry.collapsed} id={unitElementId(entry.unit.id)} data-unit-path={entry.unit.files[0]!.path} style:--unit-row={entry.row} style:--unit-span={entry.span} aria-label={`Review unit ${entry.position}: ${entry.unit.title}`}>
                  <UnitContext unit={entry.unit} position={entry.position!} total={units!.length} wide={guideWide} {viewportHeight} {snapshot} generation={store.state?.analysis?.generation} collapsed={entry.collapsed} ontoggle={() => setUnitCollapsed(entry.unit!.id, !entry.collapsed)} />
                </section>
              {/if}
              <div class="file-slot" hidden={entry.collapsed} style:--file-row={entry.row}><FileDiff file={entry.file} /></div>
            {/each}
            <p class="end">End of changes <span aria-hidden="true">·</span> {store.files.length} file{store.files.length === 1 ? '' : 's'}</p>
          </div>
        {/key}
      {/if}
    {/if}
  </div>
  <QuestionPanel />
</div>

<style>
  .diff-viewport { display: flex; flex-direction: column; height: 100%; min-height: 0; min-width: 0; }
  .diff-content { flex: 1; min-height: 0; overflow-y: auto; overflow-x: hidden; scrollbar-gutter: stable; overscroll-behavior-y: contain; }
  .stream { display: flex; flex-direction: column; gap: 16px; padding: 12px 16px 24px; }
  .empty { padding: 48px 24px; text-align: center; }
  .discussions-error { margin: 12px 16px; }
  .discussions-error p { margin: 0 0 8px; }
  .stream[hidden] { display: none; }
  .stream.guide { display: grid; grid-template-columns: clamp(260px, 30%, 360px) minmax(0, 1fr); gap: 20px 24px; padding-top: 20px; }
  .unit-context, .file-slot { min-width: 0; }
  .unit-context { padding-top: 8px; scroll-margin-top: 16px; }
  .guide .unit-context { grid-column: 1; grid-row: var(--unit-row) / span var(--unit-span); align-self: stretch; }
  .guide .unit-context.collapsed { grid-column: 1 / -1; }
  .unit-context.collapsed { padding-top: 0; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-subtle); }
  .file-slot[hidden] { display: none; }
  .guide .file-slot { grid-column: 2; grid-row: var(--file-row); align-self: start; }
  .guide .end { grid-column: 1 / -1; }
  .end { text-align: center; color: var(--fg-muted); font-size: 12px; margin: 12px 0 0; }
  @media (max-width: 800px) { .stream { padding: 12px 8px 24px; gap: 12px; } .unit-context { padding-inline: 8px; } }
</style>
