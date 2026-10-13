<script lang="ts">
  import { markdownFileLinks, renderMarkdown, type FileReference } from '../lib/markdown';
  import { attachmentId } from '../lib/attachments';
  import { store } from '../lib/store.svelte';
  /** `images` is for human-authored text only (see renderMarkdown). */
  let { text, images = false }: { text: string; images?: boolean } = $props();
  const paths = $derived(store.files.map((file) => file.path));
  const attachments = $derived(images && /github\.com\/user-attachments\/assets\/|user-images\.githubusercontent\.com\//.test(text));
  const markdown = $derived(renderMarkdown(text, paths, { images, signed: attachments ? (src) => store.signedAttachment(src) : undefined }));
  $effect(() => { if (attachments) void store.loadAttachments(); });
  function selectFile(reference: FileReference) {
    store.selectFile(reference.path, reference.line === undefined ? {} : { line: { side: 'RIGHT', line: reference.line } });
  }
  /** Signed URLs expire after about five minutes; a failed attachment asks for fresh ones. */
  function imageFailed(event: Event) {
    const img = event.target;
    if (attachments && img instanceof HTMLImageElement && (attachmentId(img.src) || attachmentId(img.closest('a')?.href ?? ''))) void store.loadAttachments();
  }
</script>

<div class="msg" use:markdownFileLinks={{ references: markdown.references, paths, selectFile }} onerrorcapture={imageFailed}>
  {@html markdown.html}
</div>

<style>
  .msg { min-width: 0; max-width: 100%; overflow-wrap: anywhere; white-space: normal; line-height: 1.6; }
  .msg :global(> :first-child) { margin-top: 0; }
  .msg :global(> :last-child) { margin-bottom: 0; }
  .msg :global(p), .msg :global(ul), .msg :global(ol), .msg :global(blockquote), .msg :global(pre), .msg :global(.markdown-table) { margin: 0 0 0.8em; }
  .msg :global(h1), .msg :global(h2), .msg :global(h3), .msg :global(h4), .msg :global(h5), .msg :global(h6) { line-height: 1.3; margin: 1em 0 0.5em; font-weight: 600; }
  .msg :global(h1) { font-size: 1.6em; }
  .msg :global(h2) { font-size: 1.4em; }
  .msg :global(h3) { font-size: 1.2em; }
  .msg :global(h4), .msg :global(h5), .msg :global(h6) { font-size: 1em; }
  .msg :global(ul), .msg :global(ol) { padding-left: 1.7em; }
  .msg :global(li > ul), .msg :global(li > ol) { margin: 0.2em 0; }
  .msg :global(li > p) { margin: 0.3em 0; }
  .msg :global(li + li) { margin-top: 0.2em; }
  .msg :global(blockquote) { border-left: 3px solid var(--border); padding-left: 0.9em; color: var(--fg-muted); }
  .msg :global(blockquote > :last-child) { margin-bottom: 0; }
  .msg :global(a) { color: var(--accent); text-decoration: underline; text-underline-offset: 2px; }
  .msg :global(code) { font-family: var(--mono); font-size: 0.9em; background: var(--bg-inset); padding: 0.1em 0.3em; border-radius: 4px; }
  .msg :global(pre) { max-width: 100%; background: var(--bg-inset); border: 1px solid var(--border); border-radius: 6px; padding: 10px; overflow-x: auto; white-space: pre; overflow-wrap: normal; }
  .msg :global(pre code) { background: none; padding: 0; border-radius: 0; white-space: inherit; }
  .msg :global(.code-suggestion) { border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; margin: 0 0 .8em; }
  .msg :global(.suggestion-label) { padding: 6px 10px; font-size: 11px; font-weight: 600; color: var(--fg-muted); background: var(--bg-subtle); }
  .msg :global(.code-suggestion pre) { margin: 0; border: 0; border-top: 1px solid var(--border); border-radius: 0; background: var(--add-bg); }
  .msg :global(.suggestion-deletion) { padding: 8px 10px; margin: 0; color: var(--del-fg); background: var(--del-bg); font-size: 12px; }
  .msg :global(.markdown-table) { max-width: 100%; overflow-x: auto; }
  .msg :global(table) { border-collapse: collapse; width: max-content; min-width: 100%; }
  .msg :global(th), .msg :global(td) { border: 1px solid var(--border); padding: 6px 10px; min-width: 6em; overflow-wrap: normal; }
  .msg :global(th) { background: var(--bg-subtle); font-weight: 600; }
  .msg :global(tr:nth-child(even)) { background: var(--bg-subtle); }
  .msg :global(input[type='checkbox']) { margin: 0 0.4em 0 0; padding: 0; vertical-align: middle; accent-color: var(--accent); }
  .msg :global(hr) { border: 0; border-top: 1px solid var(--border); margin: 1em 0; }
  .msg :global(img) { max-width: 100%; height: auto; vertical-align: middle; border-radius: 4px; }
</style>
