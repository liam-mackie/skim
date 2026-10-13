<script lang="ts">
  import { tick, untrack } from 'svelte';
  import { store } from '../lib/store.svelte';
  import { snapshotKey } from '../lib/concurrency';
  import Message from './Message.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import RotateCw from '@lucide/svelte/icons/rotate-cw';
  import Settings from '@lucide/svelte/icons/settings';

  /** The author's description plus, outside Guide, review-unit status. Guide shows its own status panel. */
  let { scrollContainer, guide = false }: { scrollContainer?: HTMLElement; guide?: boolean } = $props();
  const id = $props.id();
  let collapsedSnapshot = $state('');
  let element = $state<HTMLElement>();
  const snapshot = $derived(snapshotKey(store.pr));
  const open = $derived(collapsedSnapshot !== snapshot);
  const task = $derived(store.state?.analysis?.grouping);
  const preparing = $derived(store.analysisRefreshing || task?.status === 'running');
  const failed = $derived(task?.status === 'failed' || task?.status === 'cancelled');
  const configured = $derived(!!store.state?.settings.roles.review.provider && !!store.state?.settings.roles.review.model);
  const blocked = $derived(store.blockReason('refresh'));
  const body = $derived(store.pr?.body.trim() ?? '');
  const unavailable = $derived(!preparing && !failed && !store.state?.groups.length);
  const canRetry = $derived(!preparing && (failed || unavailable || !!store.analysisError) && store.files.length > 0);
  let previousSnapshot = '';

  // Keep the code being read in place when asynchronous status above it changes height.
  $effect.pre(() => {
    void preparing;
    void task?.error;
    void store.analysisError;
    const sameSnapshot = previousSnapshot === snapshot;
    previousSnapshot = snapshot;
    untrack(() => {
      const scroller = scrollContainer;
      const context = element;
      if (!sameSnapshot || !scroller || !context || context.getBoundingClientRect().bottom > scroller.getBoundingClientRect().top) return;
      const before = context.offsetHeight;
      const top = scroller.scrollTop;
      void tick().then(() => {
        // Native scroll anchoring or a user scroll may already have moved the viewport.
        if (context.isConnected && scroller.scrollTop === top) scroller.scrollTop += context.offsetHeight - before;
      });
    });
  });
</script>

<section class="context" aria-label="Pull request context" bind:this={element}>
  <h2>
    <Button variant="ghost" size="sm" class="-ml-2 gap-1.5 text-xs font-semibold" aria-expanded={open} aria-controls={`${id}-content`} onclick={() => (collapsedSnapshot = open ? snapshot : '')}>
      <ChevronDown class={['size-3.5 text-muted-foreground', !open && '-rotate-90']} aria-hidden="true" />
      Author description
    </Button>
  </h2>
  <div id={`${id}-content`}>
    {#if open}
      {#if body}<div class="author"><Message text={body} images /></div>{:else}<p class="muted">The author provided no description.</p>{/if}
    {/if}
  </div>
  {#if !guide}
    {#if preparing}
      <p class="status" role="status">Preparing review units…</p>
    {:else if failed}
      <p class="status failure" role="alert">Review units {task?.status === 'cancelled' ? 'were cancelled' : 'could not be prepared'}{task?.error ? `: ${task.error}` : '.'}</p>
    {:else if unavailable}
      <p class="status">{!store.files.length ? 'No changed files to group.' : !configured ? 'Review model not configured.' : 'No review units for this snapshot.'}</p>
    {/if}
    {#if store.analysisError}<p class="status failure" role="alert">Analysis unavailable: {store.analysisError}</p>{/if}
    {#if canRetry}
      <div class="retry">
        <IconButton label={failed || store.analysisError ? 'Retry analysis' : 'Re-run analysis'} text={failed || store.analysisError ? 'Retry' : 'Re-run'} tooltip={blocked ?? 'Re-run review units and risk analysis'} size="sm" onclick={() => store.refreshAnalysis()} disabled={!!blocked}><RotateCw /></IconButton>
        {#if !configured}<IconButton label="Open Settings" text="Settings" size="sm" onclick={() => (store.settingsOpen = true)}><Settings /></IconButton>{/if}
        {#if blocked}<span class="muted" role="status">{blocked}</span>{/if}
      </div>
    {/if}
  {/if}
</section>

<style>
  .context { padding: 10px 16px 12px; border-bottom: 1px solid var(--border); font-size: 13px; overflow-wrap: anywhere; overflow-anchor: none; }
  h2 { min-width: 0; margin: 0; }
  p { margin: 6px 0 0; }
  .author { color: var(--fg-muted); }
  .status { color: var(--fg-muted); font-size: 12px; line-height: 1.5; }
  .failure { color: var(--danger); }
  .retry { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 10px; margin-top: 8px; font-size: 12px; }
  .retry span { flex-basis: 100%; }
</style>
