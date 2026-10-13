<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { verificationActive } from '../lib/concurrency';
  import type { VerificationJob } from '../lib/api';
  import PermissionRequest from './PermissionRequest.svelte';
  import X from '@lucide/svelte/icons/x';
  import ListX from '@lucide/svelte/icons/list-x';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import IconButton from '$lib/components/IconButton.svelte';
  import { Button } from '$lib/components/ui/button/index.js';

  const needsPermission = (job: VerificationJob) => store.permissions.some((permission) => permission.jobId === job.id);
  // Newest first; cleared jobs leave Activity but keep their threads and reports.
  const visible = $derived(store.verificationJobs.filter((job) => !job.cleared || verificationActive(job) || needsPermission(job))
    .sort((a, b) => b.startedAt.localeCompare(a.startedAt)));
  const awaiting = $derived(visible.filter(needsPermission));
  const groups = $derived(([
    ['active', 'In progress', (job: VerificationJob) => verificationActive(job)],
    ['failed', 'Failed', (job: VerificationJob) => job.status === 'failed'],
    ['cancelled', 'Cancelled', (job: VerificationJob) => job.status === 'cancelled'],
    ['complete', 'Complete', (job: VerificationJob) => job.status === 'complete'],
  ] as const).map(([id, label, match]) => ({ id, label, jobs: visible.filter((job) => !needsPermission(job) && match(job)) })).filter((group) => group.jobs.length));
  const unattached = $derived(store.permissions.filter((permission) => !visible.some((job) => job.id === permission.jobId)));
  const finished = $derived(visible.some((job) => !verificationActive(job) && !needsPermission(job)));
</script>

{#snippet jobCard(job: VerificationJob)}
  {@const permissions = store.permissions.filter((permission) => permission.jobId === job.id)}
  <article class="job" aria-label={`Check: ${job.question}`} data-job-id={job.id}>
    <div class="status" role="status">
      {#if verificationActive(job)}<span class="spinner"></span>{/if}
      <strong>{job.status}</strong>
    </div>
    <p class="question">{job.question}</p>
    <p class="hint mono">{job.prKey} · {job.path}:{job.startLine}{job.endLine !== job.startLine ? `–${job.endLine}` : ''} · {job.side}</p>
    {#if job.error}<div class="error" role="alert">{job.error}</div>{/if}
    {#if job.suggestionsError}<div class="warn" role="status">Report saved; actionable suggestions unavailable: {job.suggestionsError}</div>{/if}
    {#if store.verificationErrors[job.id]}<div class="error" role="alert">{store.verificationErrors[job.id]}</div>{/if}
    <div class="my-3 flex flex-wrap gap-2">
      <Button variant="outline" size="sm" onclick={() => store.openVerification(job.id)} disabled={!!store.blockReason('load') && store.pr?.url !== job.prUrl}>Open thread</Button>
      {#if verificationActive(job)}<Button variant="outline" size="sm" onclick={() => store.cancelVerification(job.id)} disabled={job.status === 'cancelling' || store.verificationCancelling[job.id]}>{job.status === 'cancelling' ? 'Cancelling…' : store.verificationCancelling[job.id] ? 'Requesting cancellation…' : 'Cancel check'}</Button>{/if}
    </div>
    {#if job.status === 'cancelling'}<p class="hint">Waiting for the agent and its checkout lease to stop safely.</p>{/if}
    {#each permissions as permission (permission.id)}<PermissionRequest {permission} />{/each}
    <details>
      <summary class="hint">Check details</summary>
      <p class="hint">Started {new Date(job.startedAt).toLocaleString()}{#if job.finishedAt} · Finished {new Date(job.finishedAt).toLocaleString()}{/if}</p>
      <p class="hint mono">{job.baseSha.slice(0, 7)}…{job.headSha.slice(0, 7)} · {job.id}</p>
      {#if job.workspace?.path}<p class="hint">Checkout: <code>{job.workspace.path}</code></p>{/if}
      {#if job.workspace?.error}<p class="error">{job.workspace.error}</p>{/if}
    </details>
  </article>
{/snippet}

<aside id="verification-activity" class="activity" hidden={!store.activityOpen} aria-labelledby="activity-title">
  <div class="flex items-center justify-between gap-3">
    <h2 id="activity-title">Verification activity</h2>
    <div class="flex items-center gap-1">
      {#if finished}
        <Button variant="ghost" size="sm" onclick={() => store.clearActivity()} disabled={store.activityClearing}><ListX class="size-4" aria-hidden="true" />{store.activityClearing ? 'Clearing…' : 'Clear finished'}</Button>
      {/if}
      <IconButton label="Close activity" onclick={() => (store.activityOpen = false)}><X class="size-4" aria-hidden="true" /></IconButton>
    </div>
  </div>
  {#if store.verificationPollingError}<div class="error" role="alert">{store.verificationPollingError}</div>{/if}
  {#if store.errors.permission}<div class="error" role="alert">{store.errors.permission}</div>{/if}
  {#if store.activityClearError}<div class="error" role="alert">{store.activityClearError}</div>{/if}
  {#if store.verificationPollingError || store.errors.permission}
    <IconButton label="Refresh activity" onclick={() => { void store.pollVerifications(); void store.pollPermissions(); }}><RefreshCw class="size-4" aria-hidden="true" /></IconButton>
  {/if}
  {#if !visible.length && !unattached.length}<p class="muted">{store.verificationJobs.length ? 'Nothing running. Cleared checks stay in their threads.' : 'No checks yet.'}</p>{/if}
  {#if awaiting.length || unattached.length}
    <section aria-labelledby="activity-permission">
      <h3 id="activity-permission">Needs permission <span class="count">{awaiting.length + unattached.length}</span></h3>
      {#each awaiting as job (job.id)}{@render jobCard(job)}{/each}
      {#each unattached as permission (permission.id)}<PermissionRequest {permission} />{/each}
    </section>
  {/if}
  {#each groups as group (group.id)}
    <section aria-labelledby={`activity-${group.id}`}>
      <h3 id={`activity-${group.id}`}>{group.label} <span class="count">{group.jobs.length}</span></h3>
      {#each group.jobs as job (job.id)}{@render jobCard(job)}{/each}
    </section>
  {/each}
</aside>

<style>
  .activity { position: absolute; z-index: 30; right: 16px; top: 64px; width: min(540px, calc(100% - 32px)); max-height: calc(100% - 80px); overflow: auto; padding: 16px; background: var(--bg); border: 1px solid var(--border); border-radius: var(--radius-lg); box-shadow: var(--shadow); overflow-wrap: anywhere; }
  .activity[hidden] { display: none; }
  h2 { margin: 0; font-size: 18px; }
  .job { border-top: 1px solid var(--border); padding: 16px 0; }
  h3 { margin: 20px 0 0; font-size: 12px; font-weight: 600; letter-spacing: .04em; text-transform: uppercase; color: var(--fg-muted); }
  .count { margin-left: 4px; font-variant-numeric: tabular-nums; }
  .status { display: flex; align-items: center; font-size: 12px; text-transform: capitalize; }
  .question { margin: 8px 0; font-weight: 600; }
  details { margin-top: 8px; }
</style>
