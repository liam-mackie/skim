<script lang="ts">
  import { store } from '../lib/store.svelte';
  import type { Permission } from '../lib/api';
  import { Button } from '$lib/components/ui/button/index.js';
  let { permission: p, compact = false }: { permission: Permission; compact?: boolean } = $props();
</script>

<section class="permission" aria-label="Agent permission request" data-permission-id={p.id}>
  <h4>Permission needed: {p.title}</h4>
  {#if !compact || p.path}<p class="hint">{#if !compact}{p.prKey ?? 'Unknown review'}{#if p.path} · {/if}{/if}{#if p.path}<span class="mono">{p.path}</span>{/if}</p>{/if}
  {#if p.question && !compact}<p class="question">{p.question}</p>{/if}
  <p class="hint">Awaiting your decision for this request only.</p>
  {#if p.details}
    <pre class="action" role="region" aria-label="Action details">{p.details}</pre>
  {:else}
    <div class="warn">The agent supplied no details for this action. Deny it unless you understand what it does.</div>
  {/if}
  {#if store.permissionErrors[p.id]}<div class="error" role="alert">{store.permissionErrors[p.id]}</div>{/if}
  <div class="my-3 flex flex-wrap gap-2">
    {#each p.options as option (option.optionId)}
      <Button variant={option.kind.startsWith('reject') ? 'destructive' : option.kind === 'allow_once' ? 'default' : 'outline'} disabled={store.permissionSubmitting[p.id]} onclick={() => store.answerPermission(p.id, option.optionId)} title={option.kind}>{option.name}</Button>
    {/each}
  </div>
  <details><summary class="hint">Request identity</summary><p class="hint mono">Job {p.jobId ?? 'not supplied'} · Thread {p.threadId ?? 'not supplied'} · Session {p.sessionId}</p></details>
</section>

<style>
  .permission { border: 1px solid var(--accent); border-radius: var(--radius); background: var(--bg); padding: 12px; margin: 12px 0; overflow-wrap: anywhere; }
  h4 { margin: 0 0 8px; font-size: 14px; }
  .question { font-size: 13px; }
  .action { background: var(--bg-subtle); border: 1px solid var(--border); border-radius: var(--radius); padding: 12px; max-height: 30vh; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-family: var(--mono); font-size: 12px; }
</style>
