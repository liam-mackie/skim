<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { snapshotKey } from '../lib/concurrency';
  import type { ReviewEvent } from '../lib/api';
  import CommentCard from './CommentCard.svelte';
  import X from '@lucide/svelte/icons/x';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';
  import * as NativeSelect from '$lib/components/ui/native-select/index.js';

  let dialog: HTMLDialogElement | undefined = $state();
  let heading: HTMLHeadingElement | undefined = $state();
  let summaries = $state<Record<string, string>>({});
  const context = $derived(store.finishReviewContext);
  const key = $derived(snapshotKey(context));
  const summary = $derived(summaries[key] ?? '');
  const pending = $derived(store.pendingComments);
  const choices: { event: ReviewEvent; label: string }[] = [
    { event: 'APPROVE', label: 'Approve' },
    { event: 'COMMENT', label: 'Comment only' },
    { event: 'REQUEST_CHANGES', label: 'Needs changes' },
  ];
  const blocked = $derived(store.finishReviewStale
    ? 'The pull request or its base/head changed. Close this overview and reopen Finish Review for the current code. Your summary is kept for the original snapshot.'
    : store.publicationBlockReason);
  const missingSummary = $derived(store.reviewEvent === 'REQUEST_CHANGES' && !summary.trim());
  const emptyComment = $derived(store.reviewEvent === 'COMMENT' && !pending.length && !summary.trim());

  $effect(() => {
    if (!dialog) return;
    if (context && !dialog.open) { dialog.showModal(); heading?.focus(); }
    if (!context && dialog.open) dialog.close();
  });

  function close() {
    if (!store.busy.publish) store.finishReviewContext = null;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const expected = context;
    const submitted = summary;
    const submittedKey = key;
    if (!expected || blocked || missingSummary || emptyComment) return;
    if (await store.publish(store.reviewEvent, submitted, expected)) {
      if (summaries[submittedKey] === submitted) delete summaries[submittedKey];
      if (store.finishReviewContext === expected) store.finishReviewContext = null;
    }
  }
</script>

<dialog bind:this={dialog} aria-labelledby="finish-review-title" oncancel={(event) => { event.preventDefault(); close(); }} onclose={close}>
  {#if context}
    <header class="flex items-center justify-between gap-3">
      <h2 id="finish-review-title" tabindex="-1" bind:this={heading}>Finish Review</h2>
      <IconButton label="Close Finish Review" onclick={close} disabled={store.busy.publish}><X class="size-4" aria-hidden="true" /></IconButton>
    </header>
    <p class="context"><strong>{context.owner}/{context.repo} #{context.number}</strong> — {context.title}</p>
    <p class="hint">Only pending comments are submitted. Unapproved suggestions stay local.</p>
    {#if blocked}<div class="warn" role="status">{blocked}</div>{/if}
    {#if !store.finishReviewStale}
      <section aria-labelledby="pending-overview-title">
        <h3 id="pending-overview-title">Local pending comments ({pending.length})</h3>
        {#if pending.length}
          {#each pending as comment (comment.id)}<CommentCard c={comment} showLocation />{/each}
        {/if}
      </section>
    {/if}
    {#if store.errors.comment}<div class="error" role="alert">{store.errors.comment}</div>{/if}
    <form id="finish-review-form" onsubmit={submit}>
      <div class="my-5 space-y-2">
        <label class="block font-semibold" for="review-outcome">Review outcome</label>
        <NativeSelect.Root class="w-full" id="review-outcome" name="review-outcome" value={store.reviewEvent} onchange={(event) => { store.reviewEvent = event.currentTarget.value as ReviewEvent; }} disabled={store.busy.publish}>
          {#each choices as choice}<option value={choice.event}>{choice.label}</option>{/each}
        </NativeSelect.Root>
      </div>
      <label for="finish-review-summary">Review summary {store.reviewEvent === 'REQUEST_CHANGES' ? '(required)' : '(optional)'}</label>
      <Textarea class="mt-2" id="finish-review-summary" rows={4} value={summary} oninput={(event) => { summaries[key] = event.currentTarget.value; }} required={store.reviewEvent === 'REQUEST_CHANGES'} disabled={store.busy.publish} />
      {#if missingSummary}<p class="hint" role="status">Needs changes requires a summary explaining what should change.</p>{/if}
      {#if emptyComment}<p class="hint" role="status">Add a pending comment or summary, or select Approve for a bare approval.</p>{/if}
      {#if store.errors.publish}<div class="error" role="alert">{store.errors.publish}</div>{/if}
      <footer class="mt-5 flex flex-wrap items-center justify-end gap-3 border-t pt-4">
        <Button variant="outline" onclick={close} disabled={store.busy.publish}>Keep reviewing</Button>
        <Button type="submit" disabled={!!blocked || missingSummary || emptyComment}>{#if store.busy.publish}<span class="spinner"></span>Submitting…{:else}Submit review to GitHub{/if}</Button>
      </footer>
    </form>
  {/if}
</dialog>

<style>
  dialog { margin: auto; width: 760px; max-width: calc(100vw - 32px); max-height: 88dvh; overflow: auto; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg); color: var(--fg); padding: 24px; box-shadow: var(--shadow); }
  dialog::backdrop { background: rgba(1, 4, 9, 0.65); backdrop-filter: blur(3px); }
  h2 { margin: 0; font-size: 20px; letter-spacing: -.3px; }
  h3 { margin: 24px 0 8px; font-size: 15px; }
  .context { overflow-wrap: anywhere; }
  @media (max-width: 600px) { dialog { padding: 20px; } }
</style>
