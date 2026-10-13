<script lang="ts">
  import { tick } from 'svelte';
  import { appendSuggestion, editSuggestion, findSuggestion, type SuggestionSelection } from '../lib/suggestion';
  import Message from './Message.svelte';
  import Code from '@lucide/svelte/icons/code';
  import Eye from '@lucide/svelte/icons/eye';
  import EyeOff from '@lucide/svelte/icons/eye-off';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';

  let {
    body = $bindable(''), selection, inputId, label = 'Comment body', rows = 5, readonly = false,
    textarea = $bindable<HTMLTextAreaElement | null>(null), onkeydown,
  }: {
    body: string; selection: SuggestionSelection; inputId: string; label?: string; rows?: number; readonly?: boolean;
    textarea?: HTMLTextAreaElement | null; onkeydown?: (event: KeyboardEvent) => void;
  } = $props();
  let preview = $state(false);
  let replacing = $state(false);
  let replacementInput = $state<HTMLTextAreaElement | null>(null);
  const existing = $derived(findSuggestion(body));

  function openSuggestion() {
    if (readonly || (!existing && selection.code === null)) return;
    if (!existing && selection.code !== null) body = appendSuggestion(body, selection.code);
    replacing = true;
    preview = false;
    void tick().then(() => replacementInput?.focus());
  }
</script>

<div class="comment-editor">
  <div hidden={preview || (replacing && !!existing)}>
    <label for={inputId}>{label}</label>
    <Textarea id={inputId} {rows} bind:ref={textarea} bind:value={body} {onkeydown} {readonly} required placeholder="Comment…" />
  </div>
  <div class="mt-2 flex flex-wrap gap-2">
    <IconButton label={existing ? 'Edit code suggestion' : 'Insert code suggestion'} text="Suggestion" tooltip={!existing ? selection.reason ?? 'Insert code suggestion' : 'Edit code suggestion'} size="sm" variant="outline" onclick={openSuggestion} disabled={readonly || (replacing && !!existing) || (!existing && selection.reason !== null)}><Code class="size-4" aria-hidden="true" /></IconButton>
    <IconButton label={preview ? 'Hide preview' : 'Preview'} text="Preview" size="sm" variant="outline" onclick={() => (preview = !preview)} aria-pressed={preview} aria-controls={`${inputId}-preview`}>{#if preview}<EyeOff class="size-4" aria-hidden="true" />{:else}<Eye class="size-4" aria-hidden="true" />{/if}</IconButton>
  </div>
  {#if selection.reason}<p class="hint" role="status">{selection.reason}</p>{/if}
  {#if replacing && existing}
    <div class="replacement-editor" hidden={preview}>
      <label for={`${inputId}-replacement`}>Replacement code</label>
      <Textarea id={`${inputId}-replacement`} class="font-mono" rows={5} bind:ref={replacementInput} value={existing.code} oninput={(event) => { if (!readonly && existing) body = editSuggestion(body, existing, event.currentTarget.value); }} {onkeydown} readonly={readonly} spellcheck="false" />
      <p class="hint">Replaces the selected new-side lines; empty deletes them. Keep indentation. No source files are changed.</p>
      <Button variant="outline" size="sm" onclick={() => { replacing = false; void tick().then(() => textarea?.focus()); }}>Done</Button>
    </div>
  {/if}
  <div id={`${inputId}-preview`} class="comment-preview" hidden={!preview}>
    {#if preview}
      <strong>Comment preview</strong>
      {#if body}<Message text={body} />{:else}<p class="hint">No preview.</p>{/if}
    {/if}
  </div>
</div>

<style>
  label { display: block; margin-bottom: 8px; }
  .hint { margin: 8px 0; }
  .replacement-editor, .comment-preview { border: 1px solid var(--border); border-radius: var(--radius); padding: 12px; margin-top: 12px; }
  .replacement-editor { background: var(--bg-subtle); }
  .comment-preview > strong { display: block; margin-bottom: 10px; font-size: 12px; color: var(--fg-muted); }
  [hidden] { display: none !important; }
</style>
