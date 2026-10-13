<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import Message from './Message.svelte';
  import ScanSearch from '@lucide/svelte/icons/scan-search';
  import ArrowLeft from '@lucide/svelte/icons/arrow-left';
  import Activity from '@lucide/svelte/icons/activity';
  import Send from '@lucide/svelte/icons/send';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';
  import CommentCard from './CommentCard.svelte';
  import PermissionRequest from './PermissionRequest.svelte';
  import { verificationActive } from '../lib/concurrency';
  const threads = $derived(store.verifyThreads);
  const active = $derived(threads.find((t) => t.id === store.activeVerifyId) ?? null);
  const comments = $derived(active ? store.comments.filter((c) => c.threadId === active.id) : []);
  const job = $derived(active ? store.verificationForThread(active.id) : undefined);
  const permissions = $derived(active ? store.permissions.filter((permission) => permission.threadId === active.id && permission.prKey === job?.prKey) : []);
  const blocked = $derived(active ? store.verificationThreadBlockReason(active.id) : store.verificationBlockReason);
  const range = (t: { startLine?: number; endLine?: number }) =>
    t.startLine === t.endLine ? `${t.endLine}` : `${t.startLine}–${t.endLine}`;

  async function open(id: string | null) {
    store.activeVerifyId = id;
    store.tab = 'verify';
    store.mobileView = 'panel';
    await tick();
    const pane = document.getElementById('tabpanel');
    if (pane) pane.scrollTop = 0;
  }
  async function cont(e: SubmitEvent) {
    e.preventDefault();
    const t = active;
    if (!t?.path || !t.startLine || !t.endLine || !t.side || !store.verifyInput.trim() || t.stale) return;
    await store.verify(store.verifyInput.trim(), { path: t.path, side: t.side, start: t.startLine, end: t.endLine, hunk: -1 }, t.id);
  }
</script>

{#if !store.pr}
  <p class="muted">No pull request loaded.</p>
{:else}
  <div class="mb-3 flex flex-wrap items-center justify-between gap-3">
    {#if active}<IconButton label="Back to checks" text="Checks" size="sm" onclick={() => open(null)}><ArrowLeft class="size-4" aria-hidden="true" /></IconButton>{:else}<h2 class="m-0 text-sm">Checks</h2>{/if}
    <Button variant="ghost" size="sm" class="ml-auto" onclick={() => (store.activityOpen = true)}><Activity class="size-4" aria-hidden="true" />Activity{store.activeVerificationJobs.length ? ` (${store.activeVerificationJobs.length})` : ''}</Button>
  </div>
  {#if store.verificationPollingError}<div class="error" role="alert">{store.verificationPollingError}</div>{/if}

  {#if !active && threads.length === 0}
    <div class="empty-state">
      <ScanSearch class="size-6 text-muted-foreground" aria-hidden="true" />
      <h2>No checks yet</h2>
      <p>Select code to ask the agent.</p>
    </div>
  {:else if !active}
    <ul class="threads">
      {#each threads as t (t.id)}
        {@const task = store.verificationForThread(t.id)}
        <li>
          <Button variant="outline" class="mb-1.5 block h-auto w-full px-3 py-2.5 text-left whitespace-normal wrap-anywhere" onclick={() => open(t.id)}>
            <span class="thread-heading"><span class="mono">{t.path}:{range(t)}</span>
            {#if t.stale}<span class="badge stale">stale</span>{:else if task}<span class="badge">{store.permissions.some((permission) => permission.threadId === t.id && permission.prKey === task.prKey) ? 'needs permission' : task.status}</span>{/if}</span>
            <span class="q">{t.messages[0]?.content.slice(0, 80) ?? ''}</span>
          </Button>
        </li>
      {/each}
    </ul>
  {/if}

  {#if active}
    <header class="thread-header">
      {#if active.path && active.endLine && active.side}
        <Button variant="link" class="h-auto justify-start p-0 text-left font-mono whitespace-normal wrap-anywhere" onclick={() => store.selectFile(active.path!, { line: { side: active.side!, line: active.endLine! } })}>{active.path}:{range(active)} <span class="muted">({active.side === 'LEFT' ? 'old' : 'new'})</span></Button>
      {:else}
        <h3>Check details</h3>
      {/if}
      {#if job}
        <div class="job-status" role="status">
          {#if verificationActive(job)}<span class="spinner"></span>{/if}<strong>{permissions.length ? 'Needs permission' : job.status}</strong>
          {#if verificationActive(job)}<Button variant="outline" size="sm" onclick={() => store.cancelVerification(job.id)} disabled={job.status === 'cancelling' || store.verificationCancelling[job.id]}>{job.status === 'cancelling' ? 'Cancelling…' : 'Cancel check'}</Button>{/if}
        </div>
      {/if}
    </header>
    <details class="technical-details">
      <summary>Details</summary>
      <p class="hint mono">Thread {active.id} · Head {active.headSha}</p>
      {#if job}
        <p class="hint mono">Job {job.id}</p>
        <p class="hint">Started {job.startedAt} · Updated {job.updatedAt}</p>
        {#if job.workspace?.path}<p class="hint">Checkout used: <code>{job.workspace.path}</code></p>{/if}
      {/if}
    </details>
    {#if job}
      {#if job.error}<div class:error={job.status === 'failed'} class:hint={job.status !== 'failed'} role={job.status === 'failed' ? 'alert' : 'status'}>{job.error}</div>{/if}
      {#if job.suggestionsError}<div class="warn" role="status">Report saved; actionable suggestions unavailable: {job.suggestionsError}</div>{/if}
      {#if store.verificationErrors[job.id]}<div class="error" role="alert">{store.verificationErrors[job.id]}</div>{/if}
    {/if}
    {#each permissions as permission (permission.id)}<PermissionRequest {permission} compact />{/each}
    {#if active.stale}
      <div class="warn">This thread belongs to an earlier PR head ({active.headSha.slice(0, 7)}) and can’t be continued.</div>
    {/if}
    <ol class="conversation-list" aria-live="polite">
      {#each active.messages as m}
        <li class={m.role}><strong class="speaker">{m.role === 'user' ? 'You' : 'Agent'}</strong><Message text={m.content} /></li>
      {/each}
    </ol>
    {#if comments.length}
      <section class="outcomes" aria-label="Actionable suggestions">
        <h3>Actionable suggestions</h3>
        <p class="hint">Approve → local pending review. No code changes are applied.</p>
        {#each comments as comment (comment.id)}<CommentCard c={comment} />{/each}
        <Button variant="link" class="h-auto p-0" onclick={() => (store.tab = 'review')}>All review comments →</Button>
      </section>
    {:else if job?.status === 'complete' && !job.suggestionsError}
      <p class="hint">No new actionable findings.</p>
    {/if}
    <form onsubmit={cont}>
      <label class="sr-only" for="vc">Follow-up question</label>
      <Textarea id="vc" rows={3} bind:value={store.verifyInput} placeholder="Follow-up question…" aria-describedby={blocked ? 'follow-up-status' : undefined} required />
      {#if blocked}<p class="hint" id="follow-up-status" role="status">{blocked}</p>{/if}
      {#if store.verificationThreadErrors[active.id]}<div class="error" role="alert">{store.verificationThreadErrors[active.id]}</div>{/if}
      <div class="mt-2.5 flex justify-end"><IconButton label="Send follow-up" text="Send" variant="default" type="submit" disabled={!!blocked || !store.verifyInput.trim()}><Send class="size-4" aria-hidden="true" /></IconButton></div>
    </form>
  {/if}
{/if}

<style>
  .threads { list-style: none; padding: 0; margin: 0; }
  .thread-heading { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
  .thread-heading .badge { flex-shrink: 0; }
  .threads .q { display: block; color: var(--fg-muted); font-size: 12px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-top: 4px; }
  .technical-details { font-size: 12px; color: var(--fg-muted); margin: 8px 0 16px; overflow-wrap: anywhere; }
  h3 { font-size: 13px; margin: 12px 0 4px; overflow-wrap: anywhere; }
  .outcomes { margin: 12px 0 20px; font-size: 13px; }
  .job-status { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin: 8px 0; font-size: 12px; text-transform: capitalize; }
</style>
