<script lang="ts">
  import { store } from '../lib/store.svelte';
  import CommentCard from './CommentCard.svelte';
  import LocalCheckout from './LocalCheckout.svelte';
  import MessageSquare from '@lucide/svelte/icons/message-square';
  import { Button } from '$lib/components/ui/button/index.js';

  const groups = $derived([
    ['Pending · local', store.comments.filter((c) => c.status === 'pending')],
    ['Suggested', store.comments.filter((c) => c.status === 'suggested')],
    ['Published · GitHub', store.comments.filter((c) => c.status === 'published')],
  ] as const);
</script>

{#if !store.pr}
  <p class="muted">No pull request loaded.</p>
{:else}
  {#if store.state?.reviewRun}
    {@const run = store.state.reviewRun}
    <div class="run" role="status">
      <strong>Last agent review:</strong> {run.reviewedPaths.length} file{run.reviewedPaths.length === 1 ? '' : 's'} reviewed at
      <span class="mono">{run.headSha.slice(0, 7)}</span>{#if run.headSha !== store.pr.headSha} <span class="badge stale">earlier head</span>{/if}.
      {#if run.skippedFiles.length}
        <div class="warn">
          <strong>{run.skippedFiles.length} file{run.skippedFiles.length === 1 ? ' was' : 's were'} not reviewed</strong> (no complete patch):
          <ul>{#each run.skippedFiles as f}<li><Button variant="link" class="inline h-auto p-0 text-left font-mono text-xs whitespace-normal wrap-anywhere" onclick={() => store.selectFile(f.path)}>{f.path}</Button> — {f.reason}</li>{/each}</ul>
        </div>
      {/if}
    </div>
  {/if}
  {#if store.comments.length === 0}
    <div class="empty-state">
      <MessageSquare class="size-6 text-muted-foreground" aria-hidden="true" />
      <h2>No review comments</h2>
      <p>Pending comments stay local until you submit to GitHub.</p>
    </div>
  {/if}
  {#if store.errors.comment}<div class="error" role="alert">{store.errors.comment}</div>{/if}
  {#each groups as [title, list]}
    {#if list.length}
      <section>
        <h3>{title} ({list.length})</h3>
        {#each list as c (c.id)}<CommentCard {c} showLocation />{/each}
      </section>
    {/if}
  {/each}

  <LocalCheckout />
{/if}

<style>
  h3 { font-size: 14px; font-weight: 600; margin: 20px 0 10px; }
  .run { font-size: 12px; border: 1px solid var(--border); border-radius: var(--radius); padding: 12px; background: var(--bg-subtle); }
  .run ul { margin: 4px 0 0; padding-left: 18px; }
</style>
