<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import CommentEditor from './CommentEditor.svelte';
  import { suggestionSelection } from '../lib/suggestion';
  import X from '@lucide/svelte/icons/x';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';

  const c = $derived(store.composer);
  const stale = $derived(store.composerStale);
  const blocked = $derived(store.blockReason('createComment'));
  const suggestion = $derived(c
    ? suggestionSelection(store.files.find((file) => file.path === c.anchor.path), { path: c.anchor.path, side: c.anchor.side, startLine: c.anchor.start, endLine: c.anchor.end, hunk: c.anchor.hunk }, stale)
    : { code: null, reason: 'Select current new-side code to suggest a replacement.' });
  let textarea = $state<HTMLTextAreaElement | null>(null);

  $effect(() => {
    const focus = store.composerFocus;
    if (focus && c) void tick().then(() => textarea?.focus());
  });

  async function close() {
    if (store.busy.createComment) return;
    const id = c?.returnFocus;
    store.composer = null;
    store.composerNote = null;
    store.errors.createComment = undefined;
    await tick();
    if (id) document.getElementById(id)?.focus();
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!c || stale) return;
    const submitted = c;
    const body = c.body;
    if (await store.createComment() && c === submitted && c.body === body) await close();
  }

  function keydown(e: KeyboardEvent) {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); void close(); }
    else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      if (!blocked && !stale) (e.currentTarget as HTMLTextAreaElement | HTMLButtonElement).form?.requestSubmit();
    }
  }
</script>

{#if c}
  <form class="composer" aria-label="Inline comment" onsubmit={submit}>
    <div class="location mono">{c.anchor.path} · {c.anchor.side === 'LEFT' ? 'old (LEFT)' : 'new (RIGHT)'} {c.anchor.start === c.anchor.end ? `line ${c.anchor.end}` : `lines ${c.anchor.start}–${c.anchor.end}`}</div>
    {#if stale}
      <div class="warn" role="alert">Unattached comment: the pull request or its base/head changed. This text is preserved, but cannot be submitted against the current code. Copy it before cancelling, then select a fresh anchor.</div>
    {/if}
    {#if store.composerNote}<div class="warn" role="status">{store.composerNote}</div>{/if}
    <CommentEditor inputId="inline-body" label="Your comment" rows={4} bind:body={c.body} bind:textarea onkeydown={keydown} readonly={!!store.busy.createComment} selection={suggestion} />
    <p class="hint">Local pending comment · not published to GitHub.</p>
    {#if blocked}<p class="hint" role="status">{blocked} Your unsaved comment is kept.</p>{/if}
    <div class="flex flex-wrap items-center gap-2">
      <Button type="submit" onkeydown={keydown} aria-keyshortcuts="Control+Enter Meta+Enter" title="Add pending comment (Ctrl/⌘ + Enter)" disabled={!!blocked || stale || !c.body.trim()}>
        {store.busy.createComment ? 'Saving…' : 'Add pending comment'}
      </Button>
      <IconButton label="Cancel comment" text="Cancel" tooltip="Cancel comment (Esc)" onclick={close} onkeydown={keydown} aria-keyshortcuts="Escape" disabled={store.busy.createComment}><X class="size-4" aria-hidden="true" /></IconButton>
    </div>
    {#if store.errors.createComment}<div class="error" role="alert">{store.errors.createComment}</div>{/if}
  </form>
{/if}

<style>
  .composer { padding: 16px; margin: 12px; border: 1px solid var(--accent); border-radius: var(--radius-lg); background: var(--bg); font-family: var(--sans); font-size: 13px; box-shadow: var(--shadow); }
  .location { color: var(--fg-muted); font-size: 11px; overflow-wrap: anywhere; margin-bottom: 12px; }
  .hint { margin: 6px 0; }
  .warn { margin: 0 0 10px; }
</style>
