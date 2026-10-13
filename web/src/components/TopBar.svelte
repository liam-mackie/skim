<script lang="ts">
  import { store } from '../lib/store.svelte';
  import type { AnalysisTask } from '../lib/api';
  import Icon from './Icon.svelte';
  import ReviewProgress from './ReviewProgress.svelte';
  import FinishReviewDialog from './FinishReviewDialog.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Input } from '$lib/components/ui/input/index.js';
  import { Badge } from '$lib/components/ui/badge/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import Activity from '@lucide/svelte/icons/activity';
  import ArrowRight from '@lucide/svelte/icons/arrow-right';
  import Settings from '@lucide/svelte/icons/settings';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Sparkles from '@lucide/svelte/icons/sparkles';
  import Check from '@lucide/svelte/icons/check';
  import X from '@lucide/svelte/icons/x';
  import CircleAlert from '@lucide/svelte/icons/circle-alert';
  let { settingsOpen, onopensettings }: { settingsOpen: boolean; onopensettings: () => void } = $props();
  let url = $state('');
  async function submit(e: SubmitEvent) {
    e.preventDefault();
    if (!url.trim()) return;
    const submitted = url;
    if (await store.openPullRequest(submitted.trim()) && url === submitted) url = '';
  }
  const pr = $derived(store.pr);
  const analysis = $derived(store.state?.analysis);
  const taskText = (t: AnalysisTask) =>
    t.status === 'running' ? `${t.completed}/${t.total}` : t.status === 'complete' ? 'done' : t.status;
  const activityCount = $derived(store.permissions.length || store.activeVerificationJobs.length);
  const activityLabel = $derived(`Activity${store.permissions.length ? ` · ${store.permissions.length} need permission` : store.activeVerificationJobs.length ? ` · ${store.activeVerificationJobs.length} running` : ''}${store.verificationPollingError || store.errors.permission ? ' · unavailable' : ''}`);
</script>

{#snippet chip(label: string, t: AnalysisTask)}
  <span class="chip {t.status}" title={t.error ?? ''}>
    {#if t.status === 'running'}<span class="spinner"></span>{/if}{label} {taskText(t)}
  </span>
{/snippet}

<header class="shrink-0 border-b bg-background">
  <div class="flex flex-wrap items-center gap-3 border-b px-3 py-2 md:flex-nowrap md:gap-5 md:px-5">
    <Button variant="ghost" class="gap-2 px-0 text-lg font-bold tracking-tight" onclick={() => store.goHome()} aria-label="Skim, go to review inbox" title="Inbox"><span class="rounded-lg bg-accent p-1.5 text-[var(--accent)]"><Icon name="skim" size={20} /></span>skim</Button>
    <form onsubmit={submit} class="order-2 flex w-full gap-2 md:order-none md:ml-auto md:max-w-xl" hidden={settingsOpen}>
      <label class="sr-only" for="pr-url">GitHub pull request URL</label>
      <Input id="pr-url" type="url" placeholder="Pull request URL…" bind:value={url} disabled={store.busy.load} required />
      <IconButton label={store.busy.load ? 'Loading pull request' : 'Open pull request'} text={store.busy.load ? 'Loading…' : 'Open'} tooltip={store.blockReason('load') ?? 'Open pull request'} variant="outline" type="submit" disabled={!!store.blockReason('load') || !url.trim()}>
        {#if store.busy.load}<RefreshCw class="motion-safe:animate-spin" />{:else}<ArrowRight />{/if}
      </IconButton>
    </form>
    <div class="ml-auto flex items-center gap-1">
      <IconButton label={activityLabel} text="Activity" class={store.permissions.length ? 'text-[var(--warn-fg)]' : ''} aria-pressed={store.activityOpen} aria-expanded={store.activityOpen} aria-controls="verification-activity" onclick={() => (store.activityOpen = !store.activityOpen)}>
        {#if store.verificationPollingError || store.errors.permission}<CircleAlert />{:else}<Activity />{/if}
      </IconButton>
      {#if activityCount}<Badge variant="secondary">{activityCount}</Badge>{/if}
      <IconButton id="open-settings" label="Settings" text="Settings" aria-pressed={settingsOpen} aria-controls="settings-page" onclick={onopensettings}><Settings /></IconButton>
    </div>
  </div>
  {#if pr && !settingsOpen && store.view === 'pr'}
    <div class="flex flex-col justify-between gap-3 px-3 py-3 md:flex-row md:items-center md:gap-6 md:px-5">
      <div class="flex min-w-0 flex-1 flex-col gap-1">
        <div class="text-xs text-muted-foreground">{pr.owner} / <strong class="font-medium text-foreground">{pr.repo}</strong> <span class="ml-1">#{pr.number}</span></div>
        <a href={pr.url} target="_blank" rel="noopener noreferrer" class="text-base font-semibold tracking-tight text-foreground [overflow-wrap:anywhere] md:text-lg">{pr.title}</a>
        <div class="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
          {pr.author} <span aria-hidden="true">·</span> <span class="branch" title={pr.baseRef}>{pr.baseRef}</span> ← <span class="branch" title={pr.headRef}>{pr.headRef}</span> <span class="font-mono" title={pr.headSha}>{pr.headSha.slice(0, 7)}</span>
          {#if analysis}<span class="ml-1 inline-flex items-center gap-2 text-[11px]" aria-label="Automatic analysis status">{#if analysis.risk.status !== 'disabled'}{@render chip('Risk', analysis.risk)}{/if}{@render chip('Guide', analysis.grouping)}</span>{/if}
        </div>
      </div>
      <div class="flex shrink-0 flex-wrap items-center gap-2">
        <IconButton label="Re-run analysis" text="Analyze" tooltip={store.blockReason('refresh') ?? 'Re-run analysis'} variant="outline" onclick={() => store.refreshAnalysis()} disabled={!!store.blockReason('refresh')}><RefreshCw class={store.analysisRefreshing ? 'motion-safe:animate-spin' : ''} /></IconButton>
        <Button variant="outline" onclick={() => store.review()} disabled={!!store.blockReason('review')} title={store.blockReason('review') ?? 'Have the ACP agent review the whole pull request in a checkout'}><Sparkles />{store.busy.review ? 'Reviewing…' : 'Agent review'}</Button>
        <Button onclick={() => store.openFinishReview()}><Check /> Finish review</Button>
      </div>
    </div>
  {/if}
  {#if !settingsOpen && store.blockReason('load')}<p class="mx-4 my-2 text-xs text-muted-foreground" role="status">{store.blockReason('load')}</p>{/if}
  {#if store.errors.load && store.state}<div class="error mx-4" role="alert">{store.errors.load}</div>{/if}
  {#if store.analysisError}<div class="error mx-4" role="alert">Analysis unavailable: {store.analysisError}</div>{/if}
  {#if store.errors.review && !store.reviewProgress?.error}<div class="error mx-4" role="alert">Review failed: {store.errors.review}</div>{/if}
  {#if store.lastAcceptedJobId}
    <div class="mx-4 my-2 flex items-center gap-2 text-xs text-muted-foreground" role="status">
      <Check class="size-3.5" /> Check started
      <Button variant="link" size="sm" onclick={() => store.openVerification(store.lastAcceptedJobId!)}>Open thread</Button>
      <IconButton size="icon-sm" label="Dismiss check started notice" onclick={() => (store.lastAcceptedJobId = null)}><X /></IconButton>
    </div>
  {/if}
  <ReviewProgress />
</header>
<FinishReviewDialog />

<style>
  .branch { font-family: var(--mono); padding: 1px 5px; border-radius: 4px; background: var(--bg-subtle); max-width: 24ch; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .chip { display: inline-flex; align-items: center; color: var(--fg-muted); white-space: nowrap; }
  .chip.complete::before, .chip.failed::before, .chip.cancelled::before { content: ''; width: 6px; height: 6px; border-radius: 50%; background: var(--ok); margin-right: 6px; }
  .chip.failed, .chip.cancelled { color: var(--danger); }
  .chip.failed::before, .chip.cancelled::before { background: var(--danger); }
  .chip .spinner { margin-right: 4px; }
  [hidden] { display: none; }
  @media (max-width: 800px) { .branch { max-width: 18ch; } }
</style>
