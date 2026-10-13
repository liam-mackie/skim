<script module lang="ts">
  export type TreeIconName =
    | 'chevron' | 'folder' | 'folder-open'
    | 'file' | 'file-added' | 'file-removed' | 'file-modified' | 'file-renamed' | 'file-copied'
    | 'comment';
</script>

<script lang="ts">
  let { name }: { name: TreeIconName } = $props();
</script>

<!-- Decorative 16px icons; colour comes from currentColor (set by the parent), meaning from the parent's text/aria. -->
<svg class="ti" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false">
  {#if name === 'chevron'}
    <path d="M4.5 6.25 8 9.75l3.5-3.5" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" />
  {:else if name === 'folder'}
    <path fill="currentColor" d="M1.5 4a1.5 1.5 0 0 1 1.5-1.5h3l1.4 1.5H13A1.5 1.5 0 0 1 14.5 5.5v6.5A1.5 1.5 0 0 1 13 13.5H3A1.5 1.5 0 0 1 1.5 12z" />
  {:else if name === 'folder-open'}
    <path fill="currentColor" opacity=".55" d="M1.5 4A1.5 1.5 0 0 1 3 2.5h3l1.4 1.5H12.5A1.5 1.5 0 0 1 14 5.5V7H3.7a1 1 0 0 0-.96.72L1.5 12z" />
    <path fill="currentColor" d="M3.7 7.5h11a.75.75 0 0 1 .72.96l-1.2 4.2a1.2 1.2 0 0 1-1.15.86H2.6a.75.75 0 0 1-.72-.96l1.07-3.8a1 1 0 0 1 .75-.26z" />
  {:else if name === 'comment'}
    <path d="M3 2.75h10a1.25 1.25 0 0 1 1.25 1.25v6.5A1.25 1.25 0 0 1 13 11.75H8.4L5.25 14.25v-2.5H3A1.25 1.25 0 0 1 1.75 10.5V4A1.25 1.25 0 0 1 3 2.75z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round" />
  {:else}
    <rect x="2.75" y="1.75" width="10.5" height="12.5" rx="2.25" fill="none" stroke="currentColor" stroke-width="1.3" />
    {#if name === 'file-added'}
      <path d="M8 5.25v5.5M5.25 8h5.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
    {:else if name === 'file-removed'}
      <path d="M5.25 8h5.5" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
    {:else if name === 'file-modified'}
      <path d="M8 4.5v3M6.5 6h3M6.5 10.5h3" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" />
    {:else if name === 'file-renamed'}
      <path d="M5.25 8h5.5M8.75 6l2 2-2 2" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" />
    {:else if name === 'file-copied'}
      <rect x="5.25" y="4.75" width="4" height="4.5" rx="1" fill="none" stroke="currentColor" stroke-width="1.2" />
      <path d="M7 11h3.5a.75.75 0 0 0 .75-.75V7" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" />
    {/if}
  {/if}
</svg>

<style>
  .ti { flex: 0 0 auto; display: block; }
</style>
