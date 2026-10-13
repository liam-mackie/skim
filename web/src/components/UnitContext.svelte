<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { diffElementId } from '../lib/diff';
  import { unitElementId, type Unit } from '../lib/tree';
  import { isFlagged, riskSummary } from '../lib/risk';
  import TreeIcon from './TreeIcon.svelte';
  import ReviewedCheckbox from './ReviewedCheckbox.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';

  let { unit, position, total, wide, viewportHeight, snapshot, generation, collapsed = false, ontoggle }: {
    unit: Unit; position: number; total: number; wide: boolean; viewportHeight: number; snapshot: string; generation?: string; collapsed?: boolean; ontoggle?: () => void;
  } = $props();
  let height = $state(0);
  const bodyId = $props.id();
  const paths = $derived(unit.files.map((file) => file.path));
  const sticky = $derived(wide && height > 0 && height + 32 < viewportHeight);
</script>

<div class="unit-copy" class:sticky class:collapsed bind:clientHeight={height}>
  <div class="unit-meta"><span>Unit {String(position).padStart(2, '0')} / {String(total).padStart(2, '0')} · {unit.files.length} file{unit.files.length === 1 ? '' : 's'}</span><ReviewedCheckbox {paths} ariaLabel={`Reviewed unit ${unit.title}`} /></div>
  <div class="unit-heading grid grid-cols-[28px_minmax(0,1fr)] items-start gap-1.5">
    {#if ontoggle}<IconButton size="icon-sm" class={collapsed ? 'self-center' : 'mt-1'} onclick={ontoggle} aria-expanded={!collapsed} aria-controls={bodyId} label={`${collapsed ? 'Expand' : 'Collapse'} review unit ${unit.title}`}><span class:closed={collapsed}><TreeIcon name="chevron" /></span></IconButton>{/if}
    <h2 tabindex="-1"><a href={`#${encodeURIComponent(unitElementId(unit.id))}`} onclick={(event) => { event.preventDefault(); store.selectUnit(unit.id, { snapshot, generation }); }}>{unit.title}</a></h2>
  </div>
  <div class="unit-details" id={bodyId} hidden={collapsed}>
  {#if unit.summary}
    <div class="explanation">{#each unit.summary.split(/\n\s*\n/) as paragraph}<p>{paragraph}</p>{/each}</div>
  {/if}
  <ul class="unit-files" aria-label={`Files in ${unit.title}`}>
    {#each unit.files as file (file.path)}
      <li><Button variant="ghost" class={['h-auto min-h-8 w-full justify-start gap-2 rounded-md px-2 py-1.5 text-left text-xs', store.selectedPath === file.path && 'current bg-muted shadow-[inset_2px_0_var(--accent)]']} aria-current={store.selectedPath === file.path ? 'location' : undefined} href={`#${encodeURIComponent(diffElementId(file.path, 'file'))}`} title={file.path} onclick={(event) => { event.preventDefault(); store.selectFile(file.path); }}>
        <span class="file-icon" class:added={file.status === 'added'} class:removed={file.status === 'removed'}><TreeIcon name={file.status === 'added' ? 'file-added' : file.status === 'removed' ? 'file-removed' : file.status === 'renamed' ? 'file-renamed' : 'file-modified'} /></span>
        <span class="filename">{file.path.slice(file.path.lastIndexOf('/') + 1)}<span class="directory">{file.path.includes('/') ? file.path.slice(0, file.path.lastIndexOf('/')) : ''}</span></span>
        {#if isFlagged(file.risk)}<span class="risk {file.risk.level}" title={riskSummary(file.risk)} aria-label={riskSummary(file.risk)}></span>{/if}
      </Button></li>
    {/each}
  </ul>
  </div>
</div>

<style>
  .unit-copy { padding: 4px 12px 20px 4px; overflow-wrap: anywhere; }
  .unit-copy.sticky { position: sticky; top: 16px; }
  .unit-copy.collapsed { padding: 4px 8px 8px; }
  .closed { display: inline-flex; transform: rotate(-90deg); }
  .unit-details[hidden] { display: none; }
  .collapsed h2 { margin: 8px 0; font-size: 16px; }
  .unit-meta { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 6px; color: var(--fg-muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  h2 { grid-column: 2; min-width: 0; margin: 6px 0 12px; font-size: 18px; font-weight: 600; letter-spacing: -.2px; line-height: 1.4; }
  h2 a { color: var(--fg); text-decoration: none; }
  h2 a:hover { color: var(--accent); text-decoration: underline; }
  h2:focus-visible { outline: 2px solid var(--accent); outline-offset: 4px; border-radius: 3px; }
  .explanation { font-size: 14px; line-height: 1.7; }
  .explanation p { margin: 0 0 14px; white-space: pre-line; }
  .unit-files { margin: 22px 0 0; padding: 0; list-style: none; }
  .unit-files li + li { margin-top: 4px; }
  .file-icon { flex: 0 0 auto; display: inline-flex; color: var(--fg-muted); }
  .file-icon.added { color: var(--add-fg); }
  .file-icon.removed { color: var(--del-fg); }
  .filename { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .directory { color: var(--fg-muted); margin-left: 7px; font-size: 11px; }
  .risk { width: 6px; height: 6px; flex: 0 0 auto; border-radius: 50%; background: var(--risk-medium); }
  .risk.high { background: var(--risk-high); }
</style>
