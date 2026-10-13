<script lang="ts">
  import { store } from '../lib/store.svelte';
  import Message from './Message.svelte';
  import MessageCircle from '@lucide/svelte/icons/message-circle';
  import Send from '@lucide/svelte/icons/send';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Textarea } from '$lib/components/ui/textarea/index.js';
  import * as NativeSelect from '$lib/components/ui/native-select/index.js';
  const threads = $derived(store.globalThreads);
  const active = $derived(threads.find((t) => t.id === store.activeChatId) ?? null);
  const blocked = $derived(store.blockReason('chat'));
  const label = (i: number) => {
    const first = threads[i]?.messages[0]?.content ?? '';
    return `${i + 1}. ${first.slice(0, 40) || '(empty)'}`;
  };
  async function send(e: SubmitEvent) {
    e.preventDefault();
    if (!store.chatInput.trim() || active?.stale) return;
    await store.chat(store.chatInput.trim());
  }
  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) (e.currentTarget as HTMLTextAreaElement).form?.requestSubmit();
  }
</script>

{#if !store.pr}
  <p class="muted">No pull request loaded.</p>
{:else}
  <div class="chat-panel">
  <div class="mb-3">
    <label class="sr-only" for="thread-pick">Chat thread</label>
    <NativeSelect.Root class="w-full" id="thread-pick" value={store.activeChatId ?? ''} onchange={(e) => (store.activeChatId = e.currentTarget.value || null)}>
      <option value="">New chat</option>
      {#each threads as t, i (t.id)}<option value={t.id}>{label(i)}{t.stale ? ' (stale)' : ''}</option>{/each}
    </NativeSelect.Root>
  </div>
  <div class="history">

  {#if active}
    {#if active.stale}
      <div class="warn">This thread belongs to an earlier PR head ({active.headSha.slice(0, 7)}) and can’t be continued. Start a new chat.</div>
    {/if}
    <ol class="conversation-list" aria-live="polite">
      {#each active.messages as m}
        <li class={m.role}><strong class="speaker">{m.role === 'user' ? 'You' : 'Assistant'}</strong><Message text={m.content} /></li>
      {/each}
    </ol>
  {:else}
    <div class="empty-state">
      <MessageCircle class="size-6 text-muted-foreground" aria-hidden="true" />
      <h2>Ask about this PR</h2>
    </div>
  {/if}
  {#if store.busy.chat}<p role="status"><span class="spinner"></span>Waiting for the chat model…</p>{/if}
  {#if store.errors.chat}<div class="error" role="alert">{store.errors.chat}</div>{/if}
  </div>

  <form class="chat-composer" onsubmit={send}>
    <label class="sr-only" for="chat-in">Message</label>
    <Textarea class="max-h-55" id="chat-in" rows={3} bind:value={store.chatInput} onkeydown={onKey} aria-keyshortcuts="Control+Enter Meta+Enter" placeholder={active ? 'Reply…' : 'Ask about this PR…'} readonly={!!store.busy.chat} disabled={!!active?.stale} required />
    {#if blocked}<p class="hint" role="status">{blocked} Your message is kept.</p>{/if}
    <div class="mt-2.5 flex justify-end">
      <IconButton label="Send" text="Send" tooltip="Send (Ctrl/⌘ + Enter)" variant="default" type="submit" disabled={!!blocked || !store.chatInput.trim() || !!active?.stale}><Send class="size-4" aria-hidden="true" /></IconButton>
    </div>
  </form>
  </div>
{/if}

<style>
  .chat-panel { display: flex; flex-direction: column; flex: 1; min-height: 0; }
  .history { flex: 1; min-height: 0; overflow-y: auto; padding: 2px; }
  .history:has(.empty-state) { display: flex; align-items: center; justify-content: center; }
  .chat-composer { border-top: 1px solid var(--border); padding-top: 16px; margin-top: 12px; }
</style>
