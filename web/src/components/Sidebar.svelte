<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { store } from '../lib/store.svelte';
  import type { DiffFile, AnalysisTask } from '../lib/api';
  import { ancestorDirs, buildTree, matchesFilter, type TreeNode } from '../lib/tree';
  import { isFlagged, riskSummary } from '../lib/risk';
  import TreeIcon, { type TreeIconName } from './TreeIcon.svelte';
  import Icon from './Icon.svelte';
  import { Input } from '$lib/components/ui/input/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';

  let filter = $state('');
  let open = $state<Set<string>>(new Set());
  let openFor = '';
  let knownPaths = new Set<string>();
  let openedSelection: string | null = null;
  let navigation = $state<HTMLElement>();

  const filtering = $derived(filter.trim() !== '');
  const files = $derived(store.files.filter((f) => matchesFilter(f, filter)));
  const tree = $derived(buildTree(files));
  const analysis = $derived(store.state?.analysis);
  const riskTask = $derived(analysis?.risk);
  const riskIssues = $derived(
    new Map([...(riskTask?.skippedFiles ?? []), ...(riskTask?.failedFiles ?? [])].map((i) => [i.path, i.reason])),
  );
  const pending = $derived(store.comments.filter((c) => c.status !== 'published' || !store.publishedThreads));
  const commentCount = $derived.by(() => {
    const counts = pending.reduce((m, c) => m.set(c.path, (m.get(c.path) ?? 0) + 1), new Map<string, number>());
    for (const thread of store.publishedThreads?.threads ?? []) counts.set(thread.path, (counts.get(thread.path) ?? 0) + thread.comments.length);
    return counts;
  });

  // Initialize every folder, including compacted chains. Badge polls never undo user collapses.
  $effect(() => {
    const p = store.selectedPath;
    const key = store.pr ? `${store.pr.url}@${store.pr.baseSha}..${store.pr.headSha}` : '';
    const paths = store.files.map((f) => f.path);
    untrack(() => {
      const changed = key !== openFor;
      const next = changed ? new Set<string>() : new Set(open);
      if (changed) knownPaths = new Set();
      for (const path of paths) {
        if (!knownPaths.has(path)) for (const d of ancestorDirs(path)) next.add(d);
      }
      if (p && (changed || p !== openedSelection)) for (const d of ancestorDirs(p)) next.add(d);
      knownPaths = new Set(paths);
      openFor = key;
      openedSelection = p;
      if (next.size !== open.size || [...next].some((d) => !open.has(d))) open = next;
    });
  });

  $effect(() => {
    void store.selectedPath;
    void store.prTab;
    void filter;
    void tick().then(() => {
      const panel = navigation?.closest('aside');
      const active = navigation?.querySelector<HTMLElement>('.row[aria-current="true"]');
      if (!panel?.clientHeight || !navigation?.clientHeight || !active) return;
      const bounds = panel.getBoundingClientRect();
      const row = active.getBoundingClientRect();
      const top = bounds.top + (navigation?.querySelector('.top')?.getBoundingClientRect().height ?? 0);
      if (row.top < top) panel.scrollTop += row.top - top - 8;
      else if (row.bottom > bounds.bottom) panel.scrollTop += row.bottom - bounds.bottom + 8;
    });
  });

  function toggle(path: string) {
    const next = new Set(open);
    if (!next.delete(path)) next.add(path);
    open = next;
  }

  const statusIcon = (s: string): TreeIconName =>
    s === 'added' ? 'file-added' : s === 'removed' ? 'file-removed' : s === 'renamed' ? 'file-renamed' : s === 'copied' ? 'file-copied' : s === 'modified' || s === 'changed' ? 'file-modified' : 'file';
  const statusClass = (s: string) => (s === 'added' ? 'added' : s === 'removed' ? 'removed' : s === 'renamed' || s === 'copied' ? 'moved' : '');
  const pendingRisk = (f: DiffFile) => !f.risk && f.patchStatus === 'available' && riskTask?.status === 'running' && !riskIssues.has(f.path);
  const riskText = (f: DiffFile) =>
    isFlagged(f.risk) ? riskSummary(f.risk)
    : riskIssues.has(f.path) ? `Risk not rated: ${riskIssues.get(f.path)}` : 'Risk assessment pending';
  const fileLabel = (f: DiffFile) => {
    const parts = [f.previousPath && f.previousPath !== f.path ? `${f.previousPath} → ${f.path}` : f.path, f.status];
    parts.push(store.reviewedPaths.has(f.path) ? 'reviewed' : 'not reviewed');
    if (f.patchStatus !== 'available') parts.push(f.patchStatus === 'unavailable' ? 'no diff available' : 'diff truncated, lines cannot be selected');
    if (isFlagged(f.risk) || riskIssues.has(f.path) || pendingRisk(f)) parts.push(riskText(f));
    const c = commentCount.get(f.path);
    if (c) parts.push(`${c} comment${c === 1 ? '' : 's'}`);
    return parts.join(', ');
  };
  const taskLine = (label: string, t: AnalysisTask) => `${label} ${t.completed}/${t.total}`;
  const base = (p: string) => p.slice(p.lastIndexOf('/') + 1);
</script>

{#snippet guides(depth: number)}
  {#each { length: Math.min(depth, 10) } as _}<span class="guide" aria-hidden="true"></span>{/each}
{/snippet}

{#snippet riskMark(f: DiffFile)}
  {#if isFlagged(f.risk)}
    <span class="risk {f.risk.level}" title={riskText(f)}></span>
  {:else if !f.risk && (riskIssues.has(f.path) || pendingRisk(f))}
    <span class="risk none" title={riskText(f)}></span>
  {/if}
{/snippet}

{#snippet fileRow(f: DiffFile, depth: number)}
  <li>
    <button
      class="row"
      class:sel={store.selectedPath === f.path}
      onclick={() => store.selectFile(f.path)}
      aria-current={store.selectedPath === f.path}
      aria-label={fileLabel(f)}
      title={fileLabel(f)}
    >
      {@render guides(depth)}
      <span class="lead"></span>
      <span class="ico {statusClass(f.status)}"><TreeIcon name={statusIcon(f.status)} /></span>
      <span class="name">{base(f.path) || f.path}</span>
      {@render riskMark(f)}
      {#if store.reviewedPaths.has(f.path)}<span class="reviewed" aria-hidden="true" title="Reviewed"><Icon name="verify" size={14} /></span>{/if}
      {#if commentCount.get(f.path)}<span class="cmt"><TreeIcon name="comment" /></span>{/if}
    </button>
  </li>
{/snippet}

{#snippet nodes(list: TreeNode[], depth: number)}
  {#each list as n (n.kind === 'dir' ? `d:${n.path}` : `f:${n.path}`)}
    {#if n.kind === 'dir'}
      {@const isOpen = filtering || open.has(n.path)}
      <li>
        <button class="row" aria-expanded={isOpen} onclick={() => toggle(n.path)} title={n.path}>
          {@render guides(depth)}
          <span class="lead chev" class:closed={!isOpen}><TreeIcon name="chevron" /></span>
          <span class="ico"><TreeIcon name={isOpen ? 'folder-open' : 'folder'} /></span>
          <span class="name">{n.label}</span>
        </button>
        {#if isOpen}<ul>{@render nodes(n.children, depth + 1)}</ul>{/if}
      </li>
    {:else}
      {@render fileRow(n.file, depth)}
    {/if}
  {/each}
{/snippet}

{#snippet status(label: string, t: AnalysisTask)}
  {#if t.status === 'running'}
    <div class="status" role="status"><span class="spinner"></span>{taskLine(label, t)}</div>
  {:else if t.status === 'failed' || t.status === 'cancelled'}
    <div class="status bad" role="alert">
      <span>{label} {t.status}{t.error ? `: ${t.error}` : ''}</span>
      <IconButton label="Retry risk analysis" text="Retry" tooltip={store.blockReason('refresh') ?? 'Retry risk analysis'} size="sm" onclick={() => store.refreshAnalysis()} disabled={!!store.blockReason('refresh')}><RotateCw /></IconButton>
    </div>
  {/if}
{/snippet}

<nav class="file-navigation" aria-label="Changed files" bind:this={navigation}>
{#if !store.pr}
  <p class="empty muted">No pull request loaded.</p>
{:else}
  <div class="top sticky top-0 z-1 border-b bg-muted/40 p-3 backdrop-blur">
    <label class="sr-only" for="file-filter">Filter files</label>
    <Input id="file-filter" type="search" placeholder="Filter files…" class="h-8 text-xs" bind:value={filter} />
    <div class="mt-2 text-[11px] text-muted-foreground tabular-nums">{filtering ? `${files.length} / ${store.files.length}` : store.files.length} file{store.files.length === 1 ? '' : 's'}</div>
    {#if riskTask}{@render status('Risk', riskTask)}{/if}
  </div>

  <ul class="tree">{@render nodes(tree, 0)}</ul>
  {#if files.length === 0}<p class="empty muted">No files match.</p>{/if}
{/if}
</nav>

<style>
  .status { display: flex; gap: 6px; align-items: center; justify-content: space-between; font-size: 12px; color: var(--fg-muted); margin-top: 4px; overflow-wrap: anywhere; }
  .status.bad { color: var(--danger); align-items: flex-start; max-height: 6.5em; overflow: auto; }
  .status.bad span { flex: 1 1 auto; min-width: 0; }
  ul { list-style: none; margin: 0; padding: 0; }
  .tree { padding: 8px 0; }
  li { padding: 0 6px; }
  .tree li li { padding: 0; }
  .row { display: flex; align-items: center; width: 100%; border: 0; border-radius: 6px; background: none; padding: 0 6px 0 0; text-align: left; min-height: 32px; gap: 6px; color: var(--fg); font: inherit; font-size: 13px; }
  .row:hover { background: color-mix(in srgb, var(--fg) 6%, transparent); }
  .row.sel, .row.sel:hover { background: color-mix(in srgb, var(--fg) 9%, transparent); box-shadow: inset 2px 0 var(--accent); }
  .row:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  .guide { flex: 0 0 16px; align-self: stretch; background: linear-gradient(var(--border), var(--border)) 7px 0 / 1px 100% no-repeat; }
  .lead { flex: 0 0 16px; display: inline-flex; color: var(--fg-muted); }
  .chev { transition: transform 0.1s; }
  .chev.closed { transform: rotate(-90deg); }
  .ico { display: inline-flex; color: var(--fg-muted); }
  .ico.added { color: var(--add-fg); }
  .ico.removed { color: var(--del-fg); }
  .ico.moved { color: var(--accent); }
  .name { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .risk { flex: 0 0 auto; width: 8px; height: 8px; border-radius: 50%; margin: 0 4px; }
  .risk.high { background: var(--risk-high); }
  .risk.medium { background: var(--risk-medium); }
  .risk.none { border: 1.5px dashed var(--fg-muted); opacity: 0.7; }
  .cmt { color: var(--fg-muted); display: inline-flex; }
  .reviewed { display: inline-flex; flex: 0 0 auto; color: var(--fg-muted); }
  .empty { padding: 16px; }
</style>
