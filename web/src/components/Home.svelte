<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import { absoluteTime, relativeTime } from '../lib/text';
  import type { ReviewSummary } from '../lib/api';
  import Icon from './Icon.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Input } from '$lib/components/ui/input/index.js';
  import { Badge } from '$lib/components/ui/badge/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import ExternalLink from '@lucide/svelte/icons/external-link';
  import ListFilter from '@lucide/svelte/icons/list-filter';
  import X from '@lucide/svelte/icons/x';
  import UserRound from '@lucide/svelte/icons/user-round';

  let { visible }: { visible: boolean } = $props();

  let username = $state(store.inboxFilter.username);
  let repo = $state(store.inboxFilter.repo);
  let scroller: HTMLDivElement | undefined;
  let savedScroll = 0;
  let opening = $state<string | null>(null);

  const inbox = $derived(store.inbox);
  const pr = $derived(store.pr);
  const filtered = $derived(!!store.inboxFilter.username || !!store.inboxFilter.repo);
  const dirty = $derived(username.trim() !== store.inboxFilter.username || repo.trim() !== store.inboxFilter.repo);

  // Hidden (display: none) containers lose their scroll offset; restore it when Home is shown again.
  // Capture the offset just before the container is hidden, independent of scroll-event timing.
  $effect.pre(() => {
    if (!visible && scroller && scroller.clientHeight > 0) savedScroll = scroller.scrollTop;
  });
  $effect(() => {
    if (!visible) return;
    void tick().then(() => {
      if (scroller) scroller.scrollTop = savedScroll;
    });
  });

  function apply(e: SubmitEvent) {
    e.preventDefault();
    void store.loadInbox({ username, repo });
  }

  function clearFilters() {
    username = '';
    repo = '';
    void store.loadInbox({ username: '', repo: '' });
  }

  async function open(r: ReviewSummary) {
    if (isLoaded(r)) { store.resumePr('guide'); return; }
    opening = r.prKey;
    try {
      await store.openPullRequest(r.url);
    } finally {
      opening = null;
    }
  }

  const isLoaded = (r: ReviewSummary) => !!pr && `${pr.provider}/${pr.owner}/${pr.repo}/${pr.number}`.toLowerCase() === r.prKey.toLowerCase();
</script>

<div class="min-h-0 flex-1 overflow-auto bg-muted/40" bind:this={scroller} onscroll={() => { if (scroller && scroller.clientHeight > 0) savedScroll = scroller.scrollTop; }}>
  <main class="mx-auto max-w-[1120px] px-4 py-6 md:px-8 md:py-9" aria-labelledby="inbox-title">
    <div class="mb-6 flex flex-wrap items-center justify-between gap-4">
      <div class="flex min-w-0 flex-wrap items-center gap-3">
        <h1 id="inbox-title" class="text-2xl font-semibold tracking-tight">Inbox</h1>
        {#if inbox}
          <a class="inline-flex items-center gap-1.5 text-xs text-muted-foreground" href={inbox.user.url} target="_blank" rel="noopener noreferrer" aria-label={`Signed in as ${inbox.user.login}`}><UserRound class="size-3.5" />{inbox.user.login}</a>
        {/if}
      </div>
      <IconButton label="Refresh inbox" text="Refresh" disabled={store.inboxLoading} onclick={() => store.loadInbox()}><RefreshCw class={store.inboxLoading ? 'motion-safe:animate-spin' : ''} /></IconButton>
    </div>
    {#if pr}
      <div class="mb-5 flex items-center justify-between gap-4 rounded-xl border bg-card p-4">
        <div class="min-w-0"><strong class="block truncate font-medium">{pr.title}</strong><span class="text-xs text-muted-foreground">{pr.owner}/{pr.repo} #{pr.number}</span></div>
        <Button variant="outline" onclick={() => store.resumePr()}>Resume <ArrowRight /></Button>
      </div>
    {/if}

    <form class="mb-5 flex flex-wrap items-center gap-2 rounded-xl border bg-card p-3" onsubmit={apply} aria-label="Inbox filters">
      <label class="sr-only" for="inbox-reviewer">Reviewer / assignee</label>
      <Input id="inbox-reviewer" class="min-w-36 flex-1" bind:value={username} placeholder="Reviewer / assignee" autocomplete="off" spellcheck="false" />
      <label class="sr-only" for="inbox-repo">Repository</label>
      <Input id="inbox-repo" class="min-w-36 flex-1" bind:value={repo} placeholder="owner/repo" autocomplete="off" spellcheck="false" />
      <IconButton label="Apply filters" text="Apply" variant="outline" type="submit" disabled={store.inboxLoading || !dirty}><ListFilter /></IconButton>
      {#if filtered || username || repo}<IconButton label="Clear filters" text="Clear" onclick={clearFilters} disabled={store.inboxLoading}><X /></IconButton>{/if}
    </form>

    {#if store.errors.load && !store.state && !store.booting}<div class="error" role="alert">{store.errors.load}</div>{/if}
    {#if store.inboxError}
      <div class="error" role="alert">
        {store.inboxError}
        <div class="mt-2"><Button variant="outline" size="sm" onclick={() => store.loadInbox()} disabled={store.inboxLoading}>Retry</Button></div>
      </div>
    {/if}
    {#if inbox && !inbox.complete}
      <div class="warn" role="status">
        <strong>Incomplete results</strong>
        {#each inbox.warnings as w}<div>{w}</div>{/each}
      </div>
    {:else if inbox}
      {#each inbox.warnings as w}<div class="warn" role="status">{w}</div>{/each}
    {/if}

    {#if !inbox && store.inboxLoading}
      <p class="text-sm text-muted-foreground" role="status"><span class="spinner"></span>Loading…</p>
    {:else if inbox}
      <p class="my-3 text-xs text-muted-foreground" role="status" aria-live="polite">
        {inbox.reviews.length} pull request{inbox.reviews.length === 1 ? '' : 's'}{inbox.repo ? ` · ${inbox.repo}` : ''}{store.inboxLoading ? ' · refreshing…' : ''}{store.inboxError ? ' · last successful result' : ''}
      </p>
      {#if inbox.reviews.length === 0}
        <div class="flex flex-col items-center gap-3 rounded-xl border bg-card px-6 py-14 text-muted-foreground">
          <Icon name="verify" size={28} />
          <h2 class="text-sm font-medium text-foreground">All caught up</h2>
        </div>
      {:else}
        <ul class="overflow-hidden rounded-xl border bg-card" aria-busy={store.inboxLoading}>
          {#each inbox.reviews as r (r.prKey)}
            <li class={['flex items-center border-t first:border-t-0', isLoaded(r) && 'bg-accent/30']}>
              <Button variant="ghost" class="h-auto min-w-0 flex-1 shrink items-start justify-start gap-3 whitespace-normal rounded-none p-4 text-left" onclick={() => open(r)} disabled={!isLoaded(r) && !!store.blockReason('load')} title={!isLoaded(r) ? store.blockReason('load') ?? '' : ''} aria-label="Review {r.owner}/{r.repo} #{r.number}: {r.title}">
                <span class={['mt-0.5 shrink-0', r.draft ? 'text-muted-foreground' : 'text-[var(--ok)]']}><Icon name="pr" size={20} /></span>
                <span class="flex min-w-0 flex-col gap-1.5">
                  <span class="text-sm font-semibold [overflow-wrap:anywhere]">{r.title}</span>
                  <span class="flex flex-wrap items-center gap-1.5">
                    <span class="text-xs font-normal text-muted-foreground [overflow-wrap:anywhere]">{r.owner}/{r.repo}#{r.number}</span>
                    {#if r.requested}<Badge variant="secondary">Requested</Badge>{/if}
                    {#if r.assigned}<Badge variant="outline">Assigned</Badge>{/if}
                    {#if r.draft}<Badge variant="outline">Draft</Badge>{/if}
                    {#if isLoaded(r)}<Badge variant="outline">Open</Badge>{/if}
                  </span>
                  <span class="text-xs font-normal text-muted-foreground">{r.author} · <time datetime={r.updatedAt} title={absoluteTime(r.updatedAt)}>{relativeTime(r.updatedAt)}</time></span>
                  {#if opening === r.prKey}<span class="text-xs text-muted-foreground" role="status"><span class="spinner"></span>Loading…</span>{/if}
                </span>
              </Button>
              <IconButton class="mr-3" size="sm" label={`Open #${r.number} on GitHub`} text="GitHub" href={r.url} target="_blank" rel="noopener noreferrer"><ExternalLink /></IconButton>
            </li>
          {/each}
        </ul>
      {/if}
    {/if}
  </main>
</div>
