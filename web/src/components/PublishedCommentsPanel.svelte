<script lang="ts">
  import { store } from '../lib/store.svelte';
  import PublishedThread from './PublishedThread.svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
</script>

<div class="flex flex-wrap items-center justify-between gap-2"><h2>GitHub discussions</h2><IconButton label={store.publishedThreadsLoading ? 'Refreshing discussions' : 'Refresh discussions'} text="Refresh" onclick={() => store.loadPublishedThreads(true)} disabled={store.publishedThreadsLoading}><RefreshCw class={`size-4 ${store.publishedThreadsLoading ? 'animate-spin' : ''}`} aria-hidden="true" /></IconButton></div>
<p class="hint">Replies publish directly to GitHub.</p>
{#if store.publishedThreadsError}
  <div class="error" role="alert">
    <p>{store.publishedThreadsError}{#if store.publishedThreads} Showing the last loaded discussions.{/if}</p>
    <Button variant="outline" size="sm" onclick={() => store.pr && store.openPullRequest(store.pr.url, store.prTab)} disabled={!!store.blockReason('load')}>Reload pull request</Button>
    {#if store.blockReason('load')}<p class="hint">{store.blockReason('load')}</p>{/if}
  </div>
{/if}
{#each store.publishedThreads?.warnings ?? [] as warning}<div class="warn" role="status">{warning}</div>{/each}
{#if store.publishedThreadsLoading && !store.publishedThreads}<p class="muted" role="status"><span class="spinner"></span>Loading existing comments…</p>
{:else if store.publishedThreads?.threads.length === 0}<div class="empty-state"><h3>No GitHub discussions</h3></div>{/if}
{#each store.publishedThreads?.threads ?? [] as thread (thread.id)}<PublishedThread {thread} showLocation />{/each}

<style>
  h2 { font-size: 14px; margin: 0; }
</style>
