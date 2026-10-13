<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import X from '@lucide/svelte/icons/x';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';

  const draft = $derived(store.questionDraft);
  const blocked = $derived(store.verificationBlockReason);
  let textarea = $state<HTMLTextAreaElement | null>(null);
  $effect(() => {
    const focus = store.questionFocus;
    if (focus) void tick().then(() => textarea?.focus({ preventScroll: true }));
  });

  async function close() {
    const id = draft?.returnFocus;
    store.clearQuestion();
    await tick();
    if (id) document.getElementById(id)?.focus({ preventScroll: true });
  }

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (store.verificationBlockReason || store.questionStale) return;
    await store.askQuestion();
  }

  function keydown(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      void close();
    } else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      (e.currentTarget as HTMLTextAreaElement | HTMLButtonElement).form?.requestSubmit();
    }
  }
</script>

{#if draft}
  <form class="question-panel" aria-label="Ask about selected code" onsubmit={submit}>
    <div class="location mono">{draft.anchor.path} · {draft.anchor.side === 'LEFT' ? 'old (LEFT)' : 'new (RIGHT)'} {draft.anchor.start === draft.anchor.end ? `line ${draft.anchor.end}` : `lines ${draft.anchor.start}–${draft.anchor.end}`}</div>
    {#if store.questionStale}
      <div class="warn" id="question-stale" role="alert">Unattached question: the pull request or its base/head changed. Your text is preserved, but cannot be submitted against different code. Copy it before clearing, then select a fresh range.</div>
    {/if}
    {#if store.selectionNote}<div class="warn" role="status">{store.selectionNote}</div>{/if}
    <label for="selection-question">Question about selected code</label>
    {#if blocked && !store.questionStale}
      <p class="hint blocked" id="question-blocked" role="status">
        {blocked}
        {#if !store.state?.settings.acp.command.trim()} <Button variant="link" class="h-auto p-0" onclick={() => document.getElementById('open-settings')?.click()}>Configure agent</Button>{/if}
      </p>
    {/if}
    <Textarea class="min-h-17 max-h-40" id="selection-question" rows={2} bind:ref={textarea} bind:value={draft.question} onkeydown={keydown} aria-keyshortcuts="Control+Enter Meta+Enter Escape" placeholder="Ask about these lines…" aria-describedby={store.questionStale ? 'question-stale' : blocked ? 'question-blocked' : 'question-help'} required />
    <div class="mt-2.5 flex flex-wrap items-center gap-2">
      <Button type="submit" onkeydown={keydown} title="Ask agent (Ctrl/⌘ + Enter)" aria-keyshortcuts="Control+Enter Meta+Enter" disabled={!!blocked || store.questionStale || !draft.question.trim()}>{store.busy.verify ? 'Sending…' : 'Ask agent'}</Button>
      <IconButton label="Clear selection" text="Clear" tooltip="Clear selection (Esc)" onclick={close} onkeydown={keydown} aria-keyshortcuts="Escape"><X class="size-4" aria-hidden="true" /></IconButton>
    </div>
    <p class="hint" id="question-help">Runs in an isolated checkout. Findings become local suggestions.</p>
    {#if store.errors.permission}<div class="error" role="alert">{store.errors.permission}</div>{/if}
    {#if store.errors.verify}<div class="error" role="alert">{store.errors.verify}</div>{/if}
  </form>
{/if}

<style>
  .question-panel { padding: 16px; background: var(--bg); border-top: 2px solid var(--accent); font-size: 13px; flex-shrink: 0; max-height: 60%; overflow: auto; box-shadow: var(--shadow); }
  .location { color: var(--fg-muted); font-size: 11px; overflow-wrap: anywhere; margin-bottom: 10px; }
  label { display: block; margin-bottom: 8px; font-weight: 600; }
  .hint { margin: 8px 0 0; }
  .warn { margin: 6px 0; }
  .blocked { margin-bottom: 8px; color: var(--fg); border-left: 2px solid var(--border); padding-left: 10px; }
</style>
