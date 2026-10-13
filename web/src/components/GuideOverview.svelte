<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { snapshotKey } from '../lib/concurrency';
  import { validatedUnits } from '../lib/tree';
  import { Button } from '$lib/components/ui/button/index.js';
  import ListOrdered from '@lucide/svelte/icons/list-ordered';
  import Files from '@lucide/svelte/icons/files';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import Settings from '@lucide/svelte/icons/settings';

  /**
   * Description shows the guide summary in every state. Inside Guide (`inGuide`) this is
   * the status panel shown only while units are not available.
   */
  let { inGuide = false }: { inGuide?: boolean } = $props();
  const id = $props.id();
  const units = $derived(validatedUnits(store.state?.groups ?? [], store.files));
  const task = $derived(store.state?.analysis?.grouping);
  const preparing = $derived(store.analysisRefreshing || task?.status === 'running');
  const failed = $derived(task?.status === 'failed' || task?.status === 'cancelled');
  const configured = $derived(!!store.state?.settings.roles.review.provider && !!store.state?.settings.roles.review.model);
  const blocked = $derived(store.blockReason('refresh'));
  const count = $derived(store.files.length);
  const expected = $derived({ snapshot: snapshotKey(store.pr), generation: store.state?.analysis?.generation });
</script>

<section class="guide-overview" class:panel={inGuide} aria-labelledby={`${id}-title`}>
  {#if units && !preparing}
    <h2 id={`${id}-title`}><ListOrdered class="size-5 text-[var(--accent)]" aria-hidden="true" />Review guide</h2>
    <p class="lead">Classified into {units.length} review unit{units.length === 1 ? '' : 's'} covering {count} file{count === 1 ? '' : 's'}, in reading order.</p>
    <ol>
      {#each units as unit (unit.id)}
        <li><button type="button" class="unit-link" onclick={() => store.selectUnit(unit.id, expected)}>{unit.title}</button> <span class="muted">· {unit.files.length} file{unit.files.length === 1 ? '' : 's'}</span></li>
      {/each}
    </ol>
    <div class="actions"><Button onclick={() => store.resumePr('guide')}><ListOrdered /> Start guide</Button></div>
  {:else if preparing}
    <h2 id={`${id}-title`}><span class="spinner" aria-hidden="true"></span>Preparing the review guide</h2>
    <p class="lead" role="status">Grouping {count} changed file{count === 1 ? '' : 's'} into review units in reading order. {inGuide ? 'The guide replaces this panel as soon as it is ready.' : 'It opens in Guide as soon as it is ready.'}</p>
    <div class="actions">
      {#if inGuide}<Button variant="outline" onclick={() => store.resumePr('diff')}><Files /> Read the diff meanwhile</Button>
      {:else}<Button onclick={() => store.resumePr('guide')}><ListOrdered /> Open guide</Button>{/if}
    </div>
  {:else}
    <h2 id={`${id}-title`}><ListOrdered class="size-5 text-muted-foreground" aria-hidden="true" />Review guide unavailable</h2>
    {#if !count}
      <p class="lead">This pull request has no changed files.</p>
    {:else if !configured}
      <p class="lead">Review units need a review model. Configure one in Settings.</p>
    {:else if failed}
      <p class="lead failure" role="alert">Review units {task?.status === 'cancelled' ? 'were cancelled' : 'could not be prepared'}{task?.error ? `: ${task.error}` : '.'}</p>
    {:else}
      <p class="lead">No review units for this snapshot.</p>
    {/if}
    {#if store.analysisError}<p class="lead failure" role="alert">Analysis unavailable: {store.analysisError}</p>{/if}
    {#if count}
      <div class="actions">
        {#if configured}<Button variant="outline" onclick={() => store.refreshAnalysis()} disabled={!!blocked} title={blocked ?? 'Prepare review units again'}><RotateCw /> Retry</Button>
        {:else}<Button variant="outline" onclick={() => (store.settingsOpen = true)}><Settings /> Settings</Button>{/if}
        {#if inGuide}<span class="muted">Files are listed in folder order below.</span>
        {:else}<Button variant="ghost" onclick={() => store.resumePr('diff')}><Files /> Review diff</Button>{/if}
      </div>
      {#if blocked}<p class="muted" role="status">{blocked}</p>{/if}
    {/if}
  {/if}
</section>

<style>
  .guide-overview { padding: 24px; border: 1px solid color-mix(in srgb, var(--accent) 25%, var(--border)); border-left: 3px solid var(--accent); border-radius: var(--radius-lg); background: color-mix(in srgb, var(--accent-soft) 45%, var(--bg)); overflow-wrap: anywhere; }
  .guide-overview.panel { margin: 16px; }
  h2 { display: flex; align-items: center; gap: 10px; margin: 0; font-size: 20px; font-weight: 600; letter-spacing: -.2px; }
  .lead { margin: 12px 0 0; font-size: 15px; line-height: 1.6; }
  .failure { color: var(--danger); }
  .muted { color: var(--fg-muted); font-size: 13px; }
  ol { margin: 12px 0 0; padding-left: 22px; line-height: 1.8; list-style: decimal; }
  .unit-link { padding: 0; border: 0; background: none; color: var(--accent); font: inherit; font-weight: 500; text-align: left; cursor: pointer; }
  .unit-link:hover { text-decoration: underline; }
  .actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; margin-top: 16px; }
  .spinner { flex: 0 0 auto; }
  p.muted { margin: 8px 0 0; }
  @media (max-width: 800px) { .guide-overview { padding: 20px 16px; } .guide-overview.panel { margin: 12px 8px; } }
</style>
