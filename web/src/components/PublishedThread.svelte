<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import type { ReviewThread } from '../lib/api';
  import { absoluteTime, relativeTime } from '../lib/text';
  import Message from './Message.svelte';
  import Reply from '@lucide/svelte/icons/reply';
  import X from '@lucide/svelte/icons/x';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';
  let { thread, showLocation = false }: { thread: ReviewThread; showLocation?: boolean } = $props();
  const uid = $props.id();
  const key = $derived(store.replyKey(thread.id));
  const draft = $derived(store.replyDrafts[key]);
  const stale = $derived(store.replyStale(thread.id));
  const blocked = $derived(store.blockReason('reply'));
  const range = $derived(thread.endLine ? (thread.startLine && thread.startLine !== thread.endLine ? `${thread.startLine}–${thread.endLine}` : String(thread.endLine)) : 'file');
  const inFiles = $derived(store.files.some((f) => f.path === thread.path));
  let input = $state<HTMLTextAreaElement | null>(null);
  async function reply() {
    store.beginReply(thread.id);
    await tick();
    input?.focus();
  }
  function keys(e: KeyboardEvent) {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      (e.currentTarget as HTMLTextAreaElement).form?.requestSubmit();
    }
  }
</script>

<article class="thread" class:outdated={thread.outdated} data-thread-id={thread.id}>
  <header>
    <span class="badge">GitHub discussion</span>
    {#if thread.outdated}<span class="badge stale">Earlier diff</span>{/if}
    {#if showLocation}
      {#if inFiles}<Button variant="link" class="h-auto basis-full justify-start p-0 text-left font-mono text-xs whitespace-normal wrap-anywhere" onclick={() => store.selectFile(thread.path, !thread.outdated && thread.endLine && (thread.side === 'LEFT' || thread.side === 'RIGHT') ? { line: { side: thread.side, line: thread.endLine } } : {})}>{thread.path}:{range}</Button>
      {:else}<span class="location mono">{thread.path || 'Unknown file'}:{range}</span>{/if}
    {:else}<span class="muted">{thread.side === 'LEFT' ? 'Old' : 'New'} {thread.endLine ? `line ${range}` : 'file comment'}</span>{/if}
  </header>
  <ol>
    {#each thread.comments as comment (comment.id)}
      <li>
        <div class="byline"><strong>{comment.author || 'Deleted user'}</strong><a href={comment.url} target="_blank" rel="noopener noreferrer"><time datetime={comment.createdAt} title={absoluteTime(comment.createdAt)}>{relativeTime(comment.createdAt)}</time></a></div>
        <Message text={comment.body} images />
      </li>
    {/each}
  </ol>
  {#if thread.outdated && thread.diffHunk}
    <details><summary>Earlier diff context (not current code)</summary><pre>{thread.diffHunk}</pre></details>
  {/if}
  {#if draft}
    <form onsubmit={(e) => { e.preventDefault(); if (!stale && thread.canReply) void store.postReply(thread.id); }}>
      <label for={uid}>Reply to this discussion</label>
      <Textarea class="mt-2" id={uid} rows={3} bind:ref={input} bind:value={draft.text} readonly={draft.pending} onkeydown={keys} aria-keyshortcuts="Control+Enter Meta+Enter" required placeholder="Write a reply…" aria-describedby={`${uid}-help`} />
      <p class="hint" id={`${uid}-help`}>Publishes immediately to GitHub, not to your local pending review.</p>
      {#if stale}<div class="warn" role="alert">The PR snapshot changed. Your text is preserved; copy it or cancel and start again on the refreshed thread.</div>{/if}
      {#if draft.error}<div class="error" role="alert">{draft.error}</div>{/if}
      {#if draft.uncertain}
        <div class="warn" role="alert">
          This reply may already be on GitHub. Refresh and check the discussion before another attempt.
          <div class="mt-2.5 flex flex-wrap items-center gap-2"><IconButton label="Refresh discussions" text="Refresh" disabled={store.publishedThreadsLoading || draft.pending} onclick={() => store.loadPublishedThreads(true)}><RefreshCw class="size-4" aria-hidden="true" /></IconButton>
          {#if draft.refreshed}<Button variant="outline" onclick={() => { draft.uncertain = false; draft.error = undefined; }}>I checked — allow another attempt</Button>{/if}</div>
        </div>
      {/if}
      {#if blocked}<p class="hint" role="status">{blocked} Your reply text is kept.</p>{/if}
      <div class="mt-2.5 flex flex-wrap items-center gap-2">
        <Button type="submit" title="Post reply to GitHub (Ctrl/⌘ + Enter)" aria-keyshortcuts="Control+Enter Meta+Enter" disabled={!!blocked || stale || draft.uncertain || !thread.canReply || !draft.text.trim()}>{draft.pending ? 'Posting…' : 'Post reply to GitHub'}</Button>
        <IconButton label="Cancel reply" text="Cancel" disabled={draft.pending} onclick={() => { delete store.replyDrafts[key]; }}><X class="size-4" aria-hidden="true" /></IconButton>
      </div>
    </form>
  {:else if thread.canReply}
    <IconButton label="Reply" text="Reply" size="sm" onclick={reply}><Reply class="size-4" aria-hidden="true" /></IconButton>
  {/if}
  {#if !thread.canReply}<p class="hint">{thread.replyDisabledReason || 'Replies are unavailable for this discussion.'}</p>{/if}
</article>

<style>
  .thread { background: var(--bg); border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 14px; margin: 12px 0; font: 13px/1.5 var(--sans); overflow-wrap: anywhere; }
  .outdated { border-style: dashed; }
  header { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; font-size: 11px; }
  .location { flex-basis: 100%; overflow-wrap: anywhere; text-align: left; }
  ol { list-style: none; padding: 0; margin: 12px 0; }
  li + li { border-top: 1px solid var(--border); margin-top: 14px; padding-top: 14px; }
  .byline { display: flex; flex-wrap: wrap; align-items: baseline; gap: 8px; margin-bottom: 8px; font-size: 12px; }
  .byline a { color: var(--fg-muted); font-size: 11px; }
  form { border-top: 1px solid var(--border); padding-top: 14px; }
  details { margin: 12px 0; color: var(--fg-muted); font-size: 12px; }
  pre { font: 12px/1.5 var(--mono); white-space: pre; overflow: auto; max-height: 220px; border: 1px solid var(--border); border-radius: var(--radius); padding: 10px; background: var(--bg-subtle); }
</style>
