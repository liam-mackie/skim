<script lang="ts">
  import type { ReviewComment } from '../lib/api';
  import { store } from '../lib/store.svelte';
  import Message from './Message.svelte';
  import CommentEditor from './CommentEditor.svelte';
  import { suggestionSelection } from '../lib/suggestion';
  import Pencil from '@lucide/svelte/icons/pencil';
  import MessageSquare from '@lucide/svelte/icons/message-square';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  let { c, showLocation = false }: { c: ReviewComment; showLocation?: boolean } = $props();
  const inputId = $props.id();
  let editing = $state(false);
  let text = $state('');
  let saving = $state(false);
  const blocked = $derived(store.blockReason('comment'));
  const busy = $derived(saving || !!blocked);
  const locked = $derived(c.status === 'published');
  const range = $derived(c.startLine === c.endLine ? `${c.endLine}` : `${c.startLine}–${c.endLine}`);
  const suggestion = $derived(suggestionSelection(store.files.find((file) => file.path === c.path), c, !!c.stale || c.headSha !== store.pr?.headSha));

  function edit() {
    text = c.body;
    editing = true;
  }
  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (locked || busy) return;
    saving = true;
    const submitted = text;
    if (await store.patchComment(c.id, { body: submitted }) && text === submitted) editing = false;
    saving = false;
  }
  async function approve() {
    saving = true;
    await store.patchComment(c.id, { status: 'pending' });
    saving = false;
  }
  async function remove() {
    saving = true;
    await store.deleteComment(c.id);
    saving = false;
  }
</script>

<article class="card {c.status}" class:stale={c.stale} data-comment-id={c.id}>
  <header>
    <span class="comment-status {c.status}">{c.status === 'pending' ? 'Pending · local' : c.status}</span>
    <span class="source">{c.source === 'manual' ? 'You' : c.source === 'review' ? 'Agent review' : 'Verification'}</span>
    {#if c.stale}<span class="badge stale">stale</span>{:else if store.pr && c.headSha !== store.pr.headSha}
      <span class="badge stale" title="Written against commit {c.headSha}">earlier head {c.headSha.slice(0, 7)}</span>
    {/if}
    {#if showLocation}
      <Button variant="link" class="h-auto basis-full justify-start whitespace-normal p-0 text-left font-mono text-xs wrap-anywhere" onclick={() => store.selectFile(c.path, { line: { side: c.side, line: c.endLine } })}>
        {c.path}:{range} ({c.side === 'LEFT' ? 'old' : 'new'})
      </Button>
    {:else}
      <span class="muted small">{c.side === 'LEFT' ? 'old' : 'new'} line {range}</span>
    {/if}
    {#if c.source === 'verification' && c.threadId}
      <IconButton label="Open verification thread" text="Thread" size="sm" onclick={() => { store.activeVerifyId = c.threadId ?? null; store.tab = 'verify'; store.mobileView = 'panel'; }}><MessageSquare class="size-4" aria-hidden="true" /></IconButton>
    {/if}
  </header>
  {#if c.stale}
    <div class="warn">
      This comment belongs to earlier code (commit <span class="mono">{c.headSha.slice(0, 7)}</span>) and cannot be published.
      {#if c.status === 'pending'}Delete it and recreate it on the current diff before submitting your review.{:else if c.status === 'suggested'}It cannot be approved for the current head.{/if}
    </div>
  {/if}
  {#if editing && !locked}
    <form onsubmit={save}>
      <CommentEditor {inputId} bind:body={text} readonly={busy} selection={suggestion} />
      <div class="mt-3 flex flex-wrap gap-1.5 border-t pt-3">
        <Button size="sm" type="submit" disabled={busy || !text.trim()}>{saving ? 'Saving…' : 'Save'}</Button>
        <Button variant="outline" size="sm" onclick={() => (editing = false)} disabled={saving}>Cancel</Button>
      </div>
    </form>
  {:else}
    <Message text={c.body} />
    {#if !locked}
      <div class="mt-3 flex flex-wrap items-center gap-1.5 border-t pt-3">
        {#if c.status === 'suggested'}
          <Button variant="outline" size="sm" onclick={approve} disabled={busy || c.stale} title={c.stale ? 'Stale suggestions cannot be approved' : 'Add to local pending review; does not apply code'}>Approve</Button>
        {/if}
        <IconButton label="Edit comment" text="Edit" size="sm" onclick={edit} disabled={busy}><Pencil class="size-4" aria-hidden="true" /></IconButton>
        <Button variant="destructive" size="sm" onclick={remove} disabled={busy}>{c.status === 'suggested' ? 'Deny' : 'Delete'}</Button>
      </div>
    {/if}
  {/if}
  {#if blocked}<p class="hint" role="status">{blocked}</p>{/if}
</article>

<style>
  .card { border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 14px; background: var(--bg); margin: 12px 0; font-family: var(--sans); font-size: 13px; overflow-wrap: anywhere; }
  .card.pending { border-left: 3px solid var(--ok); }
  .card.suggested { border-left: 3px solid var(--accent); }
  .card.published { border-left: 3px solid var(--border); }
  .card.stale { border-style: dashed; }
  header { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 12px; }
  .comment-status { font-size: 11px; font-weight: 500; text-transform: capitalize; border-radius: 999px; padding: 1px 7px; background: var(--bg-inset); color: var(--fg-muted); }
  .comment-status.pending { color: var(--ok); background: var(--ok-bg); }
  .comment-status.published { color: var(--fg-muted); background: var(--bg-inset); }
  .source { font-size: 12px; font-weight: 600; }
  .small { font-size: 11px; }
  .warn { margin: 8px 0; }
</style>
