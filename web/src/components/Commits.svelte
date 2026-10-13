<script lang="ts">
  import { commitKey, store } from '../lib/store.svelte';
  import { absoluteTime, relativeTime } from '../lib/text';
  import { Button } from '$lib/components/ui/button/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';

  let { active }: { active: boolean } = $props();
  let expanded = $state<Record<string, boolean>>({});

  const pr = $derived(store.pr);
  const cs = $derived(pr ? store.commitStates[commitKey(pr)] : undefined);

  // Fetched lazily the first time the tab is shown for this exact PR/base/head.
  $effect(() => {
    if (active && pr && !cs) void store.loadCommits();
  });

  const subject = (m: string) => m.split('\n', 1)[0] || '(no message)';
  const hasMore = (m: string) => m.includes('\n');
</script>

<section class="commits" aria-labelledby="commits-title">
  <div class="head">
    <h1 id="commits-title">Commits</h1>
    <IconButton label="Refresh commits" text="Refresh" onclick={() => store.loadCommits(true)} disabled={cs?.status === 'loading'}><RefreshCw /></IconButton>
  </div>

  {#if !cs || cs.status === 'loading'}
    <p class="muted" role="status"><span class="spinner"></span>Loading commits…</p>
  {:else if cs.status === 'error'}
    <div class="error" role="alert">
      {cs.error}
      <div><Button variant="outline" size="sm" onclick={() => store.loadCommits(true)}>Retry</Button></div>
    </div>
  {:else if cs.list}
    {@const list = cs.list}
    {#each list.warnings as w}<div class="warn" role="status">{w}</div>{/each}
    {#if !list.complete && list.warnings.length === 0}
      <div class="warn" role="status">Showing {list.commits.length} of {list.total} commits. Use GitHub for the full history.</div>
    {/if}
    {#if list.commits.length === 0}
      <div class="empty"><strong>No commits returned.</strong></div>
    {:else}
      <p class="muted count" role="status">{list.commits.length}{list.total > list.commits.length ? ` of ${list.total}` : ''} commit{list.total === 1 ? '' : 's'}</p>
      <ol class="list">
        {#each list.commits as c (c.sha)}
          <li class="row">
            <div class="main">
              <div class="subject">{subject(c.message)}</div>
              {#if hasMore(c.message)}
                <IconButton size="sm" label={expanded[c.sha] ? 'Collapse commit message' : 'Expand commit message'} text={expanded[c.sha] ? 'Hide message' : 'Message'} aria-expanded={!!expanded[c.sha]} onclick={() => (expanded[c.sha] = !expanded[c.sha])}><Ellipsis /></IconButton>
                {#if expanded[c.sha]}<pre class="full">{c.message}</pre>{/if}
              {/if}
              <div class="meta muted">
                {c.authorName}{c.authorLogin ? ` (@${c.authorLogin})` : ''} committed
                <time datetime={c.authoredAt} title={absoluteTime(c.authoredAt)}>{relativeTime(c.authoredAt)}</time>
              </div>
            </div>
            <a class="sha mono" href={c.url} target="_blank" rel="noopener noreferrer" title={c.sha} aria-label="Commit {c.sha.slice(0, 7)} on GitHub (new tab)">{c.sha.slice(0, 7)} ↗</a>
          </li>
        {/each}
      </ol>
    {/if}
  {/if}
</section>

<style>
  .commits { max-width: var(--page-width); margin: 0 auto; padding: 32px; }
  .head { display: flex; flex-wrap: wrap; justify-content: space-between; align-items: center; gap: 8px; }
  h1 { font-size: 26px; letter-spacing: -.5px; margin: 0; }
  .count { font-size: 12px; margin: 12px 0; }
  .list { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border); border-radius: var(--radius-lg); overflow: hidden; box-shadow: var(--shadow); }
  .row { display: flex; flex-wrap: wrap; gap: 16px; align-items: flex-start; justify-content: space-between; padding: 18px 20px; border-top: 1px solid var(--border); }
  .row:first-child { border-top: 0; }
  .main { min-width: 0; flex: 1 1 12rem; }
  .subject { font-weight: 600; overflow-wrap: anywhere; }
  .meta { font-size: 12px; margin-top: 6px; }
  .full { margin: 12px 0; padding: 12px; background: var(--bg-subtle); border: 1px solid var(--border); border-radius: var(--radius); white-space: pre-wrap; overflow-wrap: anywhere; font-family: var(--mono); font-size: 12px; }
  .sha { font-size: 12px; white-space: nowrap; padding: 4px 8px; background: var(--accent-soft); border-radius: 6px; }
  .empty { border: 1px solid var(--border); border-radius: var(--radius-lg); padding: 40px 24px; text-align: center; }
  @media (max-width: 800px) { .commits { padding: 24px 16px; } .row { padding: 16px; } }
</style>
