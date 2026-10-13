<script lang="ts">
  import { store, type Tab } from '../lib/store.svelte';
  import ChatPanel from './ChatPanel.svelte';
  import VerifyPanel from './VerifyPanel.svelte';
  import ReviewCommentsPanel from './ReviewCommentsPanel.svelte';
  import PublishedCommentsPanel from './PublishedCommentsPanel.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import MessageCircle from '@lucide/svelte/icons/message-circle';
  import ShieldCheck from '@lucide/svelte/icons/shield-check';
  import ListChecks from '@lucide/svelte/icons/list-checks';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  let content: HTMLDivElement | undefined = $state();
  $effect(() => {
    void store.tab;
    if (content) content.scrollTop = 0;
  });

  const reviewComments = $derived(store.comments.filter((c) => c.status !== 'published').length);
  const tabs = $derived<[Tab, string][]>([
    ['chat', 'Chat'],
    ['verify', 'Verify'],
    ['review', reviewComments ? `Review (${reviewComments})` : 'Review'],
    ['comments', 'GitHub'],
  ]);
  function key(e: KeyboardEvent) {
    const i = tabs.findIndex(([t]) => t === store.tab);
    const n = e.key === 'ArrowRight' ? i + 1 : e.key === 'ArrowLeft' ? i - 1 : null;
    if (n === null) return;
    e.preventDefault();
    const t = tabs[(n + tabs.length) % tabs.length]![0];
    store.tab = t;
    document.getElementById(`tab-${t}`)?.focus();
  }
</script>

<div class="wrap">
  <div class="tabs flex shrink-0 gap-1 overflow-x-auto border-b bg-background p-1.5" role="tablist" aria-label="Review tools" tabindex="-1" onkeydown={key}>
    {#each tabs as [id, label]}
      <IconButton
        role="tab"
        id="tab-{id}"
        label={id === 'review' ? 'Review comments' : id === 'comments' ? 'GitHub comments' : label}
        tooltip={label}
        text={id === 'review' ? 'Review' : label}
        aria-describedby={id === 'review' ? 'review-comment-count' : undefined}
        aria-selected={store.tab === id}
        aria-controls="tabpanel"
        tabindex={store.tab === id ? 0 : -1}
        variant="ghost"
        class="relative h-auto min-w-0 flex-1 flex-col gap-1 px-1 py-2 text-xs"
        onclick={() => (store.tab = id)}>
        {#if id === 'chat'}<MessageCircle />{:else if id === 'verify'}<ShieldCheck />{:else if id === 'review'}<ListChecks />{:else}<MessagesSquare />{/if}
        {#if id === 'review' && reviewComments}<span class="absolute -top-0.5 right-0 rounded-full bg-primary px-1 text-[9px] leading-3.5 text-primary-foreground tabular-nums" aria-hidden="true">{reviewComments}</span>{/if}
      </IconButton>
    {/each}
  </div>
  <span id="review-comment-count" class="sr-only">{reviewComments} pending or suggested comments</span>
  {#if store.notice}<div class="ok note" role="status">{store.notice}</div>{/if}
  {#if store.cleanupWarning}
    <div class="warn note" role="alert">
      The local checkout was not removed: {store.cleanupWarning} Saved comments and any published review are unaffected.
      {#if store.tab !== 'review'}<Button variant="outline" size="sm" class="mt-2" onclick={() => (store.tab = 'review')}>Open Local checkout</Button>{/if}
    </div>
  {/if}
  <div id="tabpanel" role="tabpanel" aria-labelledby="tab-{store.tab}" class="content" class:chat={store.tab === 'chat'} bind:this={content}>
    <div class="tool-pane chat-pane" hidden={store.tab !== 'chat'}><ChatPanel /></div>
    <div class="tool-pane" hidden={store.tab !== 'verify'}><VerifyPanel /></div>
    <div class="tool-pane" hidden={store.tab !== 'review'}><ReviewCommentsPanel /></div>
    <div class="tool-pane" hidden={store.tab !== 'comments'}><PublishedCommentsPanel /></div>
  </div>
</div>

<style>
  .wrap { display: flex; flex-direction: column; height: 100%; min-height: 0; }
  .note { margin: 12px 16px 0; flex-shrink: 0; max-height: 30%; overflow: auto; }
  .content { padding: 12px; flex: 1; min-height: 0; overflow: auto; }
  .content.chat { display: flex; flex-direction: column; overflow: hidden; }
  .chat-pane { display: flex; flex-direction: column; flex: 1; min-height: 0; }
  .tool-pane[hidden] { display: none; }
</style>
