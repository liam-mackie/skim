<script lang="ts">
  import { store } from '../lib/store.svelte';
  import X from '@lucide/svelte/icons/x';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  const run = $derived(store.reviewProgress);
  const active = $derived(!!store.busy.review || run?.status === 'running');
  let now = $state(Date.now());
  let dismissed = $state<string | null>(null);
  $effect(() => {
    if (!active) return;
    now = Date.now();
    const timer = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(timer);
  });
  const elapsed = $derived(run ? Math.max(0, Math.floor(((run.finishedAt ? Date.parse(run.finishedAt) : now) - Date.parse(run.startedAt)) / 1000)) : 0);
  const phaseText = { preparing: 'Preparing the PR checkout…', reviewing: 'The agent is reviewing the pull request…', saving: 'Saving suggested comments…', finished: 'Finished' };
  const awaitingPermission = $derived(!!run && store.permissions.some((permission) => permission.jobId === run.id));
</script>

{#if active || ((run?.status === 'failed' || run?.status === 'cancelled') && dismissed !== run.id) || store.reviewProgressError}
  <section class="review-progress" aria-label="Agent review progress">
    <div class="mb-2 flex flex-wrap items-center justify-between gap-3" role="status" aria-live="polite">
      <strong class="text-[13px]">{#if active}Agent review in progress{:else if run?.status === 'failed'}Agent review failed{:else if run?.status === 'cancelled'}Agent review cancelled{:else}Review progress{/if}</strong>
      {#if run && Number.isFinite(elapsed)}<span class="muted">{Math.floor(elapsed / 60)}:{String(elapsed % 60).padStart(2, '0')} elapsed</span>{/if}
      {#if !active && run && !store.reviewProgressError}<IconButton label="Dismiss review progress" size="icon-sm" onclick={() => (dismissed = run.id)}><X class="size-4" aria-hidden="true" /></IconButton>{/if}
    </div>
    {#if active}<progress aria-label="Agent review in progress"></progress>{/if}
    {#if run}
      {#if active}<div class="counts">{phaseText[run.phase]}</div>{:else}<div class="counts">{run.findings} suggested comment{run.findings === 1 ? '' : 's'}{#if run.rejected} · {run.rejected} could not be anchored to the diff{/if}</div>{/if}
      {#if awaitingPermission}<p class="warn">The agent is waiting for permission. <Button variant="link" class="h-auto p-0 text-xs" onclick={() => (store.activityOpen = true)}>Open Activity</Button></p>{/if}
      {#if active}<p class="hint">Suggestions save when the agent finishes. Large pull requests can take several minutes.</p>{/if}
      {#if run.error}<div class="error" role="alert">{run.error}</div>{/if}
    {:else if active}<p class="hint">Starting review…</p>{/if}
    {#if store.reviewProgressError}<div class="error" role="alert">{store.reviewProgressError} <IconButton label="Check progress" text="Refresh" size="sm" onclick={() => store.refreshReviewProgress()}><RefreshCw class="size-4" aria-hidden="true" /></IconButton></div>{/if}
  </section>
{/if}

<style>
  .review-progress { margin: 0 24px 12px; padding: 12px 16px; border: 1px solid var(--border); border-radius: var(--radius); background: var(--bg-subtle); font-size: 12px; }
  progress { display: block; width: 100%; height: 7px; border: 0; border-radius: 8px; overflow: hidden; accent-color: var(--accent); margin-bottom: 8px; }
  progress::-webkit-progress-bar { background: var(--bg-inset); }
  progress::-webkit-progress-value { background: var(--accent); }
  progress::-moz-progress-bar { background: var(--accent); }
  .counts { color: var(--fg-muted); }
  .error { margin-bottom: 0; }
  @media (max-width: 800px) { .review-progress { margin: 0 16px 12px; padding: 12px; } }
</style>
