<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import Sidebar from './Sidebar.svelte';
  import DiffView from './DiffView.svelte';
  import RightPanel from './RightPanel.svelte';
  import UnitNavigation from './UnitNavigation.svelte';
  import { Checkbox } from '$lib/components/ui/checkbox/index.js';
  import * as Tooltip from '$lib/components/ui/tooltip/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import FolderTree from '@lucide/svelte/icons/folder-tree';
  import ListOrdered from '@lucide/svelte/icons/list-ordered';
  import Rows2 from '@lucide/svelte/icons/rows-2';
  import Columns2 from '@lucide/svelte/icons/columns-2';
  import PanelLeft from '@lucide/svelte/icons/panel-left';
  import PanelRight from '@lucide/svelte/icons/panel-right';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import FileDiffIcon from '@lucide/svelte/icons/file-diff';
  import FlaskConical from '@lucide/svelte/icons/flask-conical';

  const testPreferenceId = $props.id();
  // Keep the last reader mounted unchanged while Description or Commits is visible.
  let guide = $state(false);
  $effect.pre(() => {
    if (store.prTab === 'diff' || store.prTab === 'guide') guide = store.prTab === 'guide';
  });
  const testPreferenceHint = 'Collapse test-only changes. Needs the optional Jev role; without it nothing is classified as test-only. Mixed or uncertain changes stay open. Turning this on also folds previously opened tests, except active selections and editors. Reloaded file links respect this setting.';

  type Pane = 'files' | 'review';
  const defaults = { files: 280, review: 360 };
  const minimum = { files: 200, review: 280 };
  const maximum = { files: 520, review: 680 };
  const rail = 6;
  const minimumDiff = 360;
  let preferredFiles = $state(defaults.files);
  let preferredUnits = $state(220);
  let preferredReview = $state(defaults.review);
  let filesCollapsed = $state(false);
  let unitsCollapsed = $state(false);
  let reviewCollapsed = $state(false);
  let measuredWidth = $state(0);
  let width = $state(typeof window === 'undefined' ? 1440 : window.innerWidth);
  let drag = $state<{ pane: Pane; pointer: number; x: number; start: number; originalFiles: number; originalUnits: number; originalReview: number; element: HTMLInputElement } | null>(null);

  $effect(() => { if (measuredWidth > 0) width = measuredWidth; });
  const wide = $derived(width > 1200);
  const narrow = $derived(width <= 800);
  const navMinimum = $derived(guide ? 180 : minimum.files);
  const navMaximum = $derived(guide ? 400 : maximum.files);
  const navCollapsed = $derived(guide ? unitsCollapsed : filesCollapsed);
  const navLabel = $derived(guide ? 'unit navigator' : 'file tree');
  const filesVisible = $derived(narrow ? store.mobileView === 'files' : (wide || store.mobileView !== 'panel') && !navCollapsed);
  const reviewVisible = $derived(wide ? !reviewCollapsed : store.mobileView === 'panel');
  const diffVisible = $derived(wide || (narrow ? store.mobileView === 'diff' : store.mobileView !== 'panel'));
  const resizeFiles = $derived(!narrow && filesVisible && diffVisible);
  const resizeReview = $derived(wide && reviewVisible);

  const sizes = $derived.by(() => {
    const leftMin = filesVisible ? navMinimum : 0;
    const rightMin = wide && reviewVisible ? minimum.review : 0;
    let files = filesVisible ? Math.max(leftMin, Math.min(navMaximum, guide ? preferredUnits : preferredFiles)) : 0;
    let review = wide && reviewVisible ? Math.max(rightMin, Math.min(maximum.review, preferredReview)) : 0;
    const space = Math.max(0, width - minimumDiff - (resizeFiles ? rail : 0) - (resizeReview ? rail : 0));
    if (files + review > space && files + review > leftMin + rightMin) {
      const scale = Math.max(0, (space - leftMin - rightMin) / (files + review - leftMin - rightMin));
      files = leftMin + (files - leftMin) * scale;
      review = rightMin + (review - rightMin) * scale;
    }
    return { files: Math.floor(files), review: Math.floor(review) };
  });
  const columns = $derived(narrow || (!wide && !diffVisible) ? 'minmax(0, 1fr)' : `${filesVisible ? `${sizes.files}px ${rail}px ` : ''}minmax(0, 1fr)${reviewVisible ? ` ${rail}px ${sizes.review}px` : ''}`);
  const filesMax = $derived(Math.max(navMinimum, Math.min(navMaximum, width - minimumDiff - sizes.review - (resizeFiles ? rail : 0) - (resizeReview ? rail : 0))));
  const reviewMax = $derived(Math.max(minimum.review, Math.min(maximum.review, width - minimumDiff - sizes.files - (resizeFiles ? rail : 0) - (resizeReview ? rail : 0))));

  // Explicit navigation to a verification/discussion thread reveals its panel.
  $effect(() => {
    if (wide && store.mobileView === 'panel') reviewCollapsed = false;
    if (wide && store.mobileView === 'files') {
      if (guide) unitsCollapsed = false;
      else filesCollapsed = false;
    }
  });

  function setNavigationWidth(value: number) {
    if (guide) preferredUnits = value;
    else preferredFiles = value;
  }
  function setWidth(pane: Pane, requested: number) {
    const value = Math.max(pane === 'files' ? navMinimum : minimum.review, Math.min(pane === 'files' ? filesMax : reviewMax, requested));
    if (pane === 'files') {
      if (wide && reviewVisible) preferredReview = sizes.review;
      setNavigationWidth(value);
    } else {
      if (filesVisible) setNavigationWidth(sizes.files);
      preferredReview = value;
    }
  }
  async function toggle(pane: Pane) {
    finishDrag(false);
    if (!narrow && diffVisible) {
      if (filesVisible) setNavigationWidth(sizes.files);
      if (wide && reviewVisible) preferredReview = sizes.review;
    }
    if (pane === 'files') {
      if (narrow) store.mobileView = filesVisible ? 'diff' : 'files';
      else {
        if (guide) unitsCollapsed = filesVisible;
        else filesCollapsed = filesVisible;
        if (!wide && store.mobileView === 'panel') store.mobileView = 'diff';
      }
    } else if (!wide) {
      store.mobileView = reviewVisible ? 'diff' : 'panel';
    } else {
      reviewCollapsed = reviewVisible;
      if (reviewCollapsed && store.mobileView === 'panel') store.mobileView = 'diff';
    }
    await tick();
    document.getElementById(`toggle-${pane}-pane`)?.focus({ preventScroll: true });
  }
  function startDrag(event: PointerEvent, pane: Pane) {
    if (event.button !== 0 || !event.isPrimary) return;
    event.preventDefault();
    finishDrag(false);
    const element = event.currentTarget as HTMLInputElement;
    element.focus();
    element.setPointerCapture(event.pointerId);
    drag = { pane, pointer: event.pointerId, x: event.clientX, start: sizes[pane], originalFiles: preferredFiles, originalUnits: preferredUnits, originalReview: preferredReview, element };
  }
  function moveDrag(event: PointerEvent) {
    if (!drag || drag.pointer !== event.pointerId) return;
    setWidth(drag.pane, drag.start + (event.clientX - drag.x) * (drag.pane === 'files' ? 1 : -1));
  }
  function finishDrag(restore: boolean) {
    const previous = drag;
    if (!previous) return;
    drag = null;
    if (restore) {
      preferredFiles = previous.originalFiles;
      preferredUnits = previous.originalUnits;
      preferredReview = previous.originalReview;
    }
    if (previous.element.hasPointerCapture(previous.pointer)) previous.element.releasePointerCapture(previous.pointer);
  }
  function resizeKey(event: KeyboardEvent, pane: Pane) {
    const step = event.shiftKey ? 32 : 16;
    if (event.key === 'Escape' && drag) { event.preventDefault(); finishDrag(true); return; }
    if (event.key === 'Enter') { event.preventDefault(); void toggle(pane); return; }
    let next: number;
    if (event.key === 'Home') next = pane === 'files' ? navMinimum : minimum.review;
    else if (event.key === 'End') next = pane === 'files' ? filesMax : reviewMax;
    else if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') next = sizes[pane] + (event.key === 'ArrowRight' ? step : -step) * (pane === 'files' ? 1 : -1);
    else return;
    event.preventDefault();
    setWidth(pane, next);
  }
  $effect(() => () => finishDrag(false));
</script>

<div class="workspace" bind:clientWidth={measuredWidth}>
  <div class="layout-toolbar">
    <nav class="mobile-tabs" aria-label="Sections" hidden={wide}>
      {#each ([['files', guide ? 'Units' : 'File tree'], ['diff', guide ? 'Walkthrough' : 'Diff'], ['panel', 'Chat & review']] as const) as [id, label]}
        {#if id !== 'files' || narrow}
          <IconButton {label} text={id === 'panel' ? 'Review' : id === 'files' ? guide ? 'Units' : 'Files' : guide ? 'Guide' : 'Diff'} variant="ghost" class="max-[800px]:flex-1" aria-pressed={store.mobileView === id} onclick={() => { if (id === 'files') { if (guide) unitsCollapsed = false; else filesCollapsed = false; } store.mobileView = id; }}>
            {#if id === 'files'}{#if guide}<ListOrdered />{:else}<FolderTree />{/if}{:else if id === 'diff'}<FileDiffIcon />{:else}<MessagesSquare />{/if}
          </IconButton>
        {/if}
      {/each}
    </nav>
    <div class="view-controls">
      <div class="flex gap-0.5 rounded-lg border bg-muted/40 p-0.5" role="group" aria-label="Diff layout">
        <IconButton label="Unified" text="Unified" variant="ghost" size="sm" aria-pressed={store.diffLayout === 'unified'} onclick={() => (store.diffLayout = 'unified')}><Rows2 /></IconButton>
        <IconButton label="Side by side" text="Side by side" variant="ghost" size="sm" aria-pressed={store.diffLayout === 'split'} onclick={() => (store.diffLayout = 'split')}><Columns2 /></IconButton>
      </div>
    </div>
    <Tooltip.Root>
      <Tooltip.Trigger>
        {#snippet child({ props })}
          <span {...props} class="inline-flex items-center gap-2 text-xs text-muted-foreground">
            <Checkbox id={testPreferenceId} checked={store.collapseTests} onCheckedChange={(checked) => store.setCollapseTests(checked)} aria-label="Collapse tests by default" aria-describedby={`${testPreferenceId}-hint`} />
            <label for={testPreferenceId} class="m-0 inline-flex cursor-pointer items-center gap-1 text-xs font-normal"><FlaskConical class="size-3.5" aria-hidden="true" />Tests</label>
            <span id={`${testPreferenceId}-hint`} class="sr-only">{testPreferenceHint}</span>
          </span>
        {/snippet}
      </Tooltip.Trigger>
      <Tooltip.Content class="max-w-80">{testPreferenceHint}</Tooltip.Content>
    </Tooltip.Root>
    <div class="toggles" role="group" aria-label="Workspace panels">
      <IconButton id="toggle-files-pane" label={`${filesVisible ? 'Collapse' : 'Expand'} ${navLabel}`} text={guide ? 'Units' : 'Files'} aria-pressed={filesVisible} aria-expanded={filesVisible} aria-controls="review-files-pane" onclick={() => toggle('files')}><PanelLeft /></IconButton>
      <div hidden={!wide}><IconButton id="toggle-review-pane" label={reviewVisible ? 'Collapse chat and review panel' : 'Expand chat and review panel'} text="Review" aria-pressed={reviewVisible} aria-expanded={reviewVisible} aria-controls="review-tools-pane" onclick={() => toggle('review')}><PanelRight /></IconButton></div>
    </div>
  </div>
  <span class="sr-only" id="panel-resize-help">Drag to resize. Left and right arrows adjust width, Shift uses larger steps. Home and End use minimum and maximum widths. Enter collapses the panel. Escape cancels a drag.</span>
  <div class="body" class:resizing={!!drag} data-view={store.mobileView} style:grid-template-columns={columns}>
    <aside id="review-files-pane" class="side" aria-label={guide ? 'Review units' : 'Files'} hidden={!filesVisible}>
      <div hidden={guide}><Sidebar /></div>
      <div hidden={!guide}><UnitNavigation /></div>
    </aside>
    <div class="splitter-rail" hidden={!resizeFiles}>
      <input class="splitter" type="range" min={navMinimum} max={filesMax} step="1" value={sizes.files} aria-label={`Resize ${navLabel}`} aria-controls="review-files-pane" aria-valuetext={`${sizes.files} pixels wide`} aria-describedby="panel-resize-help" oninput={(e) => setWidth('files', e.currentTarget.valueAsNumber)} onpointerdown={(e) => startDrag(e, 'files')} onpointermove={moveDrag} onpointerup={() => finishDrag(false)} onpointercancel={() => finishDrag(true)} onlostpointercapture={() => finishDrag(false)} onkeydown={(e) => resizeKey(e, 'files')} />
    </div>
    <main class="main" aria-label={guide ? 'Guide' : 'Diff'} hidden={!diffVisible}><DiffView {guide} /></main>
    <div class="splitter-rail" hidden={!resizeReview}>
      <input class="splitter" type="range" min={minimum.review} max={reviewMax} step="1" value={sizes.review} aria-label="Resize chat and review panel" aria-controls="review-tools-pane" aria-valuetext={`${sizes.review} pixels wide`} aria-describedby="panel-resize-help" oninput={(e) => setWidth('review', e.currentTarget.valueAsNumber)} onpointerdown={(e) => startDrag(e, 'review')} onpointermove={moveDrag} onpointerup={() => finishDrag(false)} onpointercancel={() => finishDrag(true)} onlostpointercapture={() => finishDrag(false)} onkeydown={(e) => resizeKey(e, 'review')} />
    </div>
    <section id="review-tools-pane" class="panel" aria-label="Review tools" hidden={!reviewVisible}><RightPanel /></section>
  </div>
</div>

<style>
  .workspace { display: flex; flex-direction: column; flex: 1; min-height: 0; min-width: 0; }
  .layout-toolbar { display: flex; flex: 0 0 auto; flex-wrap: wrap; align-items: center; gap: 8px 12px; padding: 6px 12px; border-bottom: 1px solid var(--border); background: var(--bg-subtle); }
  .toggles { display: flex; justify-content: flex-end; gap: 8px; flex: 1; }
  .view-controls { display: flex; flex-wrap: wrap; gap: 8px; min-width: 0; }
  .mobile-tabs { display: flex; gap: 2px; padding: 2px; border: 1px solid var(--border); border-radius: 6px; background: var(--bg-inset); }
  .body { flex: 1; min-height: 0; min-width: 0; display: grid; }
  .side, .main, .panel { min-height: 0; min-width: 0; overflow: auto; }
  .main { overflow: hidden; }
  .side { background: var(--bg-subtle); }
  .panel { background: var(--bg); }
  .splitter-rail { position: relative; cursor: col-resize; background: var(--bg-subtle); min-height: 0; }
  .splitter-rail::before { content: ''; position: absolute; top: 0; bottom: 0; left: 2px; width: 1px; background: var(--border); pointer-events: none; }
  .splitter-rail::after { content: ''; position: absolute; width: 3px; height: 32px; top: calc(50% - 16px); left: 1px; border-radius: 3px; background: var(--fg-muted); opacity: .4; pointer-events: none; }
  .splitter-rail:hover, .splitter-rail:focus-within { background: var(--accent-soft); }
  .splitter-rail:hover::after, .splitter-rail:focus-within::after { background: var(--accent); opacity: 1; }
  .splitter { appearance: none; display: block; width: 100%; height: 100%; min-height: 0; padding: 0; margin: 0; border: 0; border-radius: 0; background: transparent; cursor: col-resize; touch-action: none; user-select: none; outline-offset: -1px; }
  .splitter::-webkit-slider-thumb { appearance: none; width: 1px; height: 1px; background: transparent; }
  .splitter::-moz-range-thumb { width: 1px; height: 1px; border: 0; background: transparent; }
  .splitter::-moz-range-track { background: transparent; }
  .resizing :global(*) { cursor: col-resize !important; user-select: none !important; }
  [hidden] { display: none !important; }
  @media (max-width: 1200px) { .toggles { flex: 0 0 auto; margin-left: auto; } }
  @media (max-width: 800px) {
    .toggles { display: none; }
    .layout-toolbar { padding: 8px 12px; gap: 8px; }
    .mobile-tabs { flex-basis: 100%; min-width: 0; gap: 2px; }
  }
</style>
