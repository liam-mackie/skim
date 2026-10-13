<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import { snapshotKey } from '../lib/concurrency';
  import { unitElementId, validatedUnits } from '../lib/tree';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import Check from '@lucide/svelte/icons/check';
  import LoaderCircle from '@lucide/svelte/icons/loader-circle';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';

  let navigation = $state<HTMLElement>();
  const units = $derived(validatedUnits(store.state?.groups ?? [], store.files));
  const task = $derived(store.state?.analysis?.grouping);
  const reviewed = $derived(store.files.filter((file) => store.reviewedPaths.has(file.path)).length);
  const saving = $derived(Object.values(store.reviewPending).some(Boolean));
  const activeId = $derived(units?.find((unit) => unit.files.some((file) => file.path === store.selectedPath))?.id);
  $effect(() => {
    void activeId;
    void tick().then(() => {
      const panel = navigation?.closest('aside');
      const link = navigation?.querySelector<HTMLElement>('a[aria-current="location"]');
      if (!navigation?.clientHeight || !panel?.clientHeight || !link) return;
      const bounds = panel.getBoundingClientRect();
      const row = link.getBoundingClientRect();
      const top = bounds.top + (navigation.querySelector('header')?.getBoundingClientRect().height ?? 0);
      if (row.top < top) panel.scrollTop += row.top - top - 8;
      else if (row.bottom > bounds.bottom) panel.scrollTop += row.bottom - bounds.bottom + 8;
    });
  });
</script>

<nav class="unit-navigation" aria-label="Review units" bind:this={navigation}>
  <header class="sticky top-0 z-1 border-b bg-muted/40 px-3 py-2 backdrop-blur">
    <h2 class="m-0 text-xs font-semibold">Review units</h2>
    <p class="mt-1 flex items-center gap-1.5 text-[11px] text-muted-foreground tabular-nums">{reviewed} / {store.files.length} reviewed{#if saving}<span role="status"><LoaderCircle class="size-3 animate-spin" aria-hidden="true" /><span class="sr-only">Saving review progress</span></span>{/if}</p>
  </header>
  {#if units}
    <ol class="m-0 list-none space-y-0.5 p-1.5">
      {#each units as unit, index (unit.id)}
        {@const count = unit.files.filter((file) => store.reviewedPaths.has(file.path)).length}
        {@const complete = count === unit.files.length}
        {@const current = unit.files.some((file) => file.path === store.selectedPath)}
        <li>
          <Button variant="ghost" class={['h-auto min-h-12 w-full items-start justify-start gap-2 rounded-md px-2 py-2 text-left whitespace-normal', current && 'current bg-accent text-accent-foreground shadow-[inset_2px_0_var(--accent)]', complete && 'complete']} aria-current={current ? 'location' : undefined} href={`#${encodeURIComponent(unitElementId(unit.id))}`} onclick={(event) => { event.preventDefault(); store.selectUnit(unit.id, { snapshot: snapshotKey(store.pr), generation: store.state?.analysis?.generation }); }}>
            <span class={['pt-px text-[11px] tabular-nums', complete ? 'text-[var(--ok)]' : 'text-muted-foreground']}>{String(index + 1).padStart(2, '0')}</span>
            <span class="min-w-0 flex-1"><strong class="block text-xs leading-5 font-medium [overflow-wrap:anywhere]">{unit.title}</strong><span class="mt-0.5 block text-[11px] text-muted-foreground tabular-nums">{count} / {unit.files.length}<span class="sr-only"> files reviewed</span></span></span>
            {#if complete}<Check class="mt-0.5 size-3.5 shrink-0 text-[var(--ok)]" aria-label="Reviewed" />{/if}
          </Button>
        </li>
      {/each}
    </ol>
  {:else if task?.status === 'running'}
    <p class="flex items-center gap-2 p-3 text-xs text-muted-foreground" role="status"><LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />Preparing review units…</p>
  {:else if task?.status === 'failed' || task?.status === 'cancelled'}
    <div class="flex items-start gap-2 p-3"><p class="m-0 min-w-0 flex-1 text-xs text-destructive [overflow-wrap:anywhere]" role="alert">{task.error || 'Review units could not be prepared.'}</p><IconButton label="Retry analysis" tooltip={store.blockReason('refresh') ?? 'Retry analysis'} size="icon-sm" onclick={() => store.refreshAnalysis()} disabled={!!store.blockReason('refresh')}><RotateCw /></IconButton></div>
  {:else}
    <p class="p-3 text-xs text-muted-foreground">No review units available.</p>
  {/if}
</nav>
