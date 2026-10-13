<script lang="ts">
  import { store } from '../lib/store.svelte';
  import Message from './Message.svelte';
  import GuideOverview from './GuideOverview.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import Files from '@lucide/svelte/icons/files';
  import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
  const pr = $derived(store.pr);
</script>

{#if pr}
  <article class="description" aria-labelledby="pr-desc-title">
    <h1 id="pr-desc-title" class="sr-only">Pull request description</h1>
    <GuideOverview />
    <section class="body" aria-labelledby="author-description-title">
      <h2 id="author-description-title">Description</h2>
      {#if pr.body.trim()}
        <Message text={pr.body} images />
      {:else}
        <p class="muted">No description.</p>
      {/if}
    </section>
    <div class="next">
      <Button variant="outline" onclick={() => store.resumePr('diff')}><Files /> Review diff</Button>
      <Button variant="outline" onclick={() => (store.prTab = 'commits')}><GitCommitHorizontal /> Commits</Button>
    </div>
  </article>
{/if}

<style>
  .description { width: 100%; min-width: 0; padding: 32px; }
  h2 { font-size: 18px; margin: 0 0 16px; font-weight: 600; letter-spacing: -.2px; }
  .body { margin-top: 24px; border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 24px; background: var(--bg); line-height: 1.6; box-shadow: var(--shadow); }
  .body p { margin: 0; }
  .next { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 24px; }
  @media (max-width: 800px) { .description { padding: 24px 16px; } .body { padding: 16px; } }
</style>
