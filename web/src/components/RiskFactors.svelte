<script lang="ts">
  import { store } from '../lib/store.svelte';
  import { factorState, riskLabel, RISK_MEANING, RISK_SCOPE, type FlaggedRisk } from '../lib/risk';

  let { path, risk }: { path: string; risk: FlaggedRisk } = $props();
  const id = $props.id();
  let open = $state(true);
  const explained = $derived(factorState(risk, store.state?.analysis?.risk));
</script>

<section class="risk-factors {risk.level}" aria-labelledby={`${id}-title`}>
  <button type="button" class="toggle" aria-expanded={open} aria-controls={`${id}-body`} onclick={() => (open = !open)}>
    <span id={`${id}-title`} class="title">{riskLabel(risk)}</span>
    <span class="muted">{Math.round(risk.probability * 100)}% confident · {explained === 'ready' ? `${risk.factors?.length} factor${risk.factors?.length === 1 ? '' : 's'} worth checking` : 'factors worth checking'}</span>
    <span class="chevron" class:closed={!open} aria-hidden="true">▾</span>
  </button>
  <div id={`${id}-body`} hidden={!open}>
    <p class="meaning">{RISK_MEANING[risk.level]}</p>
    {#if explained === 'ready'}
      <ul>
        {#each risk.factors ?? [] as factor}
          <li>
            <span>{factor.text}</span>
            <button type="button" class="line-link" onclick={() => store.selectFile(path, { line: { side: factor.side, line: factor.startLine } })}>
              {factor.side === 'LEFT' ? 'Old' : 'New'} line{factor.endLine > factor.startLine ? `s ${factor.startLine}–${factor.endLine}` : ` ${factor.startLine}`}
            </button>
          </li>
        {/each}
      </ul>
    {:else if explained === 'none'}
      <p class="muted">The review model found nothing in this diff that supports the rating. Review the file on its merits.</p>
    {:else if explained === 'pending'}
      <p class="muted" role="status">Finding the changed lines behind this rating…</p>
    {:else if explained === 'error'}
      <p class="muted" role="alert">Factors unavailable: {risk.factorsError}</p>
    {:else}
      <p class="muted">Factors were not prepared for this rating. Re-run analysis to explain it.</p>
    {/if}
    <p class="scope">{RISK_SCOPE}</p>
  </div>
</section>

<style>
  .risk-factors { margin: 0; padding: 8px 12px 10px; border-bottom: 1px solid var(--border); border-left: 3px solid var(--risk-medium); background: color-mix(in srgb, var(--risk-medium) 6%, var(--bg)); font-size: 12px; line-height: 1.5; }
  .risk-factors.high { border-left-color: var(--risk-high); background: color-mix(in srgb, var(--risk-high) 6%, var(--bg)); }
  .toggle { display: flex; align-items: baseline; gap: 8px; width: 100%; padding: 0; border: 0; background: none; color: inherit; font: inherit; text-align: left; cursor: pointer; }
  .title { font-weight: 600; color: var(--risk-medium); }
  .high .title { color: var(--risk-high); }
  .chevron { margin-left: auto; color: var(--fg-muted); transition: transform .15s; }
  .chevron.closed { transform: rotate(-90deg); }
  .muted { color: var(--fg-muted); }
  .meaning { margin: 6px 0 0; color: var(--fg-muted); }
  ul { margin: 8px 0 0; padding-left: 18px; list-style: disc; }
  li { margin: 4px 0; }
  li span { margin-right: 6px; }
  .line-link { padding: 0; border: 0; background: none; color: var(--accent); font: inherit; font-family: var(--mono); font-size: 11px; cursor: pointer; white-space: nowrap; }
  .line-link:hover { text-decoration: underline; }
  p { margin: 6px 0 0; }
  .scope { color: var(--fg-muted); font-size: 11px; }
  [hidden] { display: none !important; }
  @media (prefers-reduced-motion: reduce) { .chevron { transition: none; } }
</style>
