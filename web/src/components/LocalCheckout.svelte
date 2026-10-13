<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { Button } from '$lib/components/ui/button/index.js';

  /** Snapshot of the exact checkout the user is confirming; never re-derived from later responses. */
  let confirmingDiscard = $state<{ id: string; path?: string; count: number; error?: string } | null>(null);
  const ws = $derived(store.workspace);
  const pr = $derived(store.pr);
  const unsafe = $derived(ws?.status === 'preparing' || ws?.status === 'in_use' || ws?.status === 'cleanup_pending');
  const removable = $derived(ws?.status === 'ready' || ws?.status === 'failed' || ws?.status === 'cleanup_failed');
  const failed = $derived(ws?.status === 'cleanup_failed');
  /** Only the backend's typed dirty flag permits offering discard; ownership/path/auth errors never do. */
  const canDiscard = $derived(failed && ws?.canDiscardChanges === true && !!ws.id && (!!ws.path || (ws.count ?? 0) > 1));
  const blocked = $derived(store.blockReason('finish'));
  const disabled = $derived(!!blocked || unsafe);

  function reveal(node: HTMLElement) {
    node.scrollIntoView({ block: 'nearest' });
  }
  async function removeCheckout() {
    await store.removeCheckout();
  }
  async function discard() {
    // Only this explicit, confirmed action ever sends discardChanges:true.
    const c = confirmingDiscard;
    if (!c) return;
    if (await store.removeCheckout({ workspaceId: c.id })) confirmingDiscard = null;
  }
  // A changed PR/checkout invalidates a pending confirmation: it named another path.
  $effect(() => {
    void ws?.path;
    void store.pr?.baseSha;
    void store.pr?.headSha;
    void (store.pr && `${store.pr.provider}/${store.pr.owner}/${store.pr.repo}/${store.pr.number}`);
    void ws?.id;
    confirmingDiscard = null;
  });
</script>

{#if pr}
  <section class="finish" aria-labelledby="checkout-title">
    <h3 id="checkout-title">Local checkout{(ws?.count ?? 0) > 1 ? 's' : ''}</h3>
    <p class="hint">Removes all managed checkouts for this PR, without publishing. Comments, chats, and history stay.</p>
    {#if ws?.path && ws.status !== 'finished'}
      <p class="hint">Local checkout: <code class="mono path">{ws.path}</code></p>
    {/if}
    {#if (ws?.count ?? 0) > 1}<p class="hint">{ws?.count} isolated checkouts.</p>{/if}
    {#if ws}<p class="hint" role="status" data-testid="workspace-status">Checkout status: {ws.status.replace('_', ' ')}.</p>{/if}
    {#if failed}
      <div class="warn" role="alert">
        <strong>Local checkout cleanup is incomplete.</strong> {store.cleanupWarning}
        <p class="hint">Your review and any published comments are unaffected. Retry after fixing the cause.{#if canDiscard} If the checkout has local changes you no longer need, you can discard them.{/if}</p>
      </div>
    {/if}
    {#if store.errors.finish}<div class="error" role="alert">{store.errors.finish}</div>{/if}
    {#if store.errors.workspace}<div class="error" role="alert">{store.errors.workspace}</div>{/if}
    {#if blocked}<p class="hint" role="status">{blocked}</p>{/if}
    {#if confirmingDiscard}
      <div class="danger" role="alertdialog" aria-label="Confirm discarding local changes" use:reveal>
        <strong>Discard local changes?</strong>
        <p>Permanently delete the uncommitted changes in {#if confirmingDiscard.path}<code class="mono path">{confirmingDiscard.path}</code>{:else}these {confirmingDiscard.count} managed checkouts{/if} and remove {(confirmingDiscard.count > 1) ? 'the checkouts' : 'the checkout'}. This cannot be undone.</p>
        {#if confirmingDiscard.error}<p class="hint">Reason cleanup stopped: {confirmingDiscard.error}</p>{/if}
        <div class="mt-2 flex flex-wrap gap-2">
          <Button variant="destructive" onclick={discard} {disabled}>
            {#if store.busy.finish}<span class="spinner"></span>Removing…{:else}Discard changes and remove{/if}
          </Button>
          <Button variant="outline" onclick={() => (confirmingDiscard = null)} disabled={store.busy.finish}>Cancel</Button>
        </div>
      </div>
    {:else}
      <div class="mt-2 flex flex-wrap gap-2">
        <Button variant="outline" onclick={removeCheckout} {disabled} hidden={!removable} class={!removable ? 'hidden' : ''} data-testid="remove-checkout">
          {#if store.busy.finish}<span class="spinner"></span>Removing…{:else if failed}Retry cleanup{:else}Remove checkout{/if}
        </Button>
        {#if canDiscard}
          <Button variant="destructive" onclick={() => { if (ws?.id) confirmingDiscard = { id: ws.id, path: ws.path, count: ws.count ?? 1, error: ws.error }; }} {disabled}>Discard local changes…</Button>
        {/if}
      </div>
      {#if !removable && !unsafe}<p class="hint">No local checkout to remove{ws?.status === 'finished' ? ': this review round is finished' : ' yet'}.</p>{/if}
    {/if}
  </section>
{/if}

<style>
  .finish { border-top: 1px solid var(--border); margin-top: 24px; padding-top: 4px; }
  h3 { font-size: 14px; font-weight: 600; margin: 20px 0 10px; }
  .path { overflow-wrap: anywhere; }
  .danger { border: 1px solid var(--danger); border-radius: var(--radius); background: var(--danger-bg); padding: 12px; margin-top: 12px; }
  .danger p { margin: 6px 0; overflow-wrap: anywhere; }
</style>
