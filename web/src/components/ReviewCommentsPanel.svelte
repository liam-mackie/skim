<script lang="ts">
  import { store } from '../lib/store.svelte';
  import CommentCard from './CommentCard.svelte';
  import LocalCheckout from './LocalCheckout.svelte';
  import Message from './Message.svelte';
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
      <strong>Last agent review</strong> at <span class="mono">{run.headSha.slice(0, 7)}</span>{#if run.headSha !== store.pr.headSha} <span class="badge stale">earlier head</span>{/if}.
      {#if run.error}<div class="error">{run.error}</div>{/if}
      {#if run.rejected.length}
        <details class="warn">
          <summary><strong>{run.rejected.length} comment{run.rejected.length === 1 ? '' : 's'} could not be anchored to the diff</strong></summary>
          <ul>{#each run.rejected as c}<li><Button variant="link" class="inline h-auto p-0 text-left font-mono text-xs whitespace-normal wrap-anywhere" onclick={() => store.selectFile(c.path)}>{c.path}:{c.startLine}{c.endLine !== c.startLine ? `–${c.endLine}` : ''}</Button> — {c.reason}<p class="rejected-body">{c.body}</p></li>{/each}</ul>
        </details>
      {/if}
      <details>
        <summary>Agent report</summary>
        <Message text={run.report} />
      </details>
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
  .run details { margin-top: 8px; }
  .run summary { cursor: pointer; }
  .rejected-body { margin: 2px 0 6px; white-space: pre-wrap; color: var(--fg-muted); }
</style>
