<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { store } from './lib/store.svelte';
  import TopBar from './components/TopBar.svelte';
  import ReviewLayout from './components/ReviewLayout.svelte';
  import VerificationActivity from './components/VerificationActivity.svelte';
  import SettingsPanel from './components/SettingsPanel.svelte';
  import Home from './components/Home.svelte';
  import Description from './components/Description.svelte';
  import Commits from './components/Commits.svelte';
  import type { PrTab } from './lib/store.svelte';
  import Icon from './components/Icon.svelte';
  import { ReviewNavigation } from './lib/navigation.svelte';
  import { Button } from '$lib/components/ui/button/index.js';
  import * as Tooltip from '$lib/components/ui/tooltip/index.js';

  const navigation = new ReviewNavigation(store);
  $effect(() => navigation.sync());

  let settingsTitle: HTMLHeadingElement | undefined;
  const prTabs: [PrTab, string][] = [['guide', 'Guide'], ['diff', 'Diff'], ['description', 'Description'], ['commits', 'Commits']];

  async function openSettings() {
    store.settingsOpen = true;
    await tick();
    settingsTitle?.focus();
  }

  async function closeSettings() {
    store.settingsOpen = false;
    await tick();
    document.getElementById('open-settings')?.focus();
  }

  onMount(() => {
    void navigation.start();
    void store.loadInbox();
    return () => { navigation.dispose(); store.dispose(); };
  });
</script>

<Tooltip.Provider delayDuration={300}>
<div class="shell">
  {#if !navigation.error}<TopBar settingsOpen={store.settingsOpen} onopensettings={openSettings} />{/if}
  {#if !navigation.ready || navigation.error}
    <main class="navigation-status" aria-label="Opening review">
      {#if navigation.error}
        <h1>Could not open review</h1>
        <p class="error" role="alert">{navigation.error}</p>
        <div class="navigation-actions">
          <Button onclick={() => navigation.restore(true)} disabled={navigation.restoring}>Retry</Button>
          <Button variant="outline" onclick={() => navigation.home()}>Inbox</Button>
        </div>
      {:else}
        <p role="status"><span class="spinner"></span>Loading review…</p>
      {/if}
    </main>
  {/if}
  <div class="home-page" hidden={!navigation.ready || !!navigation.error || store.settingsOpen || store.view !== 'home'}>
    <Home visible={!store.settingsOpen && store.view === 'home'} />
  </div>
  <div class="review-workspace" hidden={!navigation.ready || !!navigation.error || store.settingsOpen || store.view !== 'pr'}>
  {#if store.pr}
    <nav class="pr-tabs" aria-label="Pull request views">
      {#each prTabs as [id, label]}
        <Button variant="ghost" class={['h-11 rounded-none border-x-0 border-t-0 border-b-2 px-3 max-[360px]:px-2', store.prTab === id ? 'border-[var(--accent)] bg-accent font-semibold text-[var(--accent)] hover:bg-accent hover:text-[var(--accent)] dark:hover:bg-accent' : 'border-transparent text-muted-foreground']} aria-current={store.prTab === id ? 'page' : undefined} onclick={() => store.resumePr(id)}>{label}{#if id === 'diff'}<span class="tab-count">{store.files.length}</span>{:else if id === 'guide' && store.state?.groups.length}<span class="tab-count" title="Review units">{store.state.groups.length}</span>{/if}</Button>
      {/each}
    </nav>
    <div class="pr-pane" hidden={store.prTab !== 'description'}><Description /></div>
    <div class="pr-pane" hidden={store.prTab !== 'commits'}><Commits active={store.view === 'pr' && !store.settingsOpen && store.prTab === 'commits'} /></div>
  {/if}
  <div class="reader-view" hidden={!!store.pr && store.prTab !== 'diff' && store.prTab !== 'guide'}>
  {#if store.booting}
    <p class="center" role="status"><span class="spinner"></span>Connecting…</p>
  {:else if !store.state}
    <div class="center">
      <div class="error" role="alert">{store.errors.load ?? 'No state available.'}</div>
      <Button variant="outline" onclick={() => navigation.restore(true)}>Retry</Button>
    </div>
  {:else}
    <ReviewLayout />
  {/if}
  </div>
  </div>
  <main id="settings-page" class="settings-page" hidden={!store.settingsOpen} aria-labelledby="settings-title">
    <div class="settings-content">
      <Button variant="ghost" class="-ml-2 text-muted-foreground" onclick={closeSettings}><Icon name="back" size={16} /> {store.view === 'home' ? 'Inbox' : 'Review'}</Button>
      <h1 id="settings-title" tabindex="-1" bind:this={settingsTitle}>Settings</h1>
      {#if store.notice}<div class="ok settings-notice" role="status">{store.notice}</div>{/if}
      {#if store.booting}
        <p class="muted">Loading settings…</p>
      {:else if store.state}
        <SettingsPanel />
      {:else}
        <div class="error" role="alert">{store.errors.load ?? 'Settings unavailable.'}</div>
        <Button variant="outline" onclick={() => navigation.restore(true)}>Retry</Button>
      {/if}
    </div>
  </main>
  <VerificationActivity />
</div>
</Tooltip.Provider>

<style>
  .shell { position: relative; display: flex; flex-direction: column; height: 100%; overflow: clip; }
  .review-workspace, .reader-view { display: flex; flex-direction: column; flex: 1; min-height: 0; }
  .home-page { flex: 1; min-height: 0; display: flex; flex-direction: column; }
  .home-page[hidden], .review-workspace[hidden], .reader-view[hidden], .pr-pane[hidden], .settings-page[hidden] { display: none; }
  .pr-tabs { display: flex; gap: 8px; padding: 0 24px; border-bottom: 1px solid var(--border); background: var(--bg); overflow-x: auto; }
  .tab-count { font-size: 11px; line-height: 18px; background: var(--bg-inset); border-radius: 12px; padding: 0 6px; font-variant-numeric: tabular-nums; }
  .pr-pane { flex: 1; min-height: 0; overflow: auto; }
  .settings-page { flex: 1; min-height: 0; overflow: auto; background: var(--bg-subtle); }
  .settings-content { max-width: var(--page-width); margin: 0 auto; padding: 32px; overflow-wrap: anywhere; }
  h1 { font-size: 28px; letter-spacing: -.6px; margin: 20px 0 24px; }
  .settings-notice { position: sticky; top: 8px; z-index: 3; }
  .center { margin: auto; text-align: center; padding: 24px; }
  .navigation-status { max-width: 680px; margin: auto; padding: 24px; overflow-wrap: anywhere; }
  .navigation-actions { display: flex; gap: 8px; }
  @media (max-width: 800px) {
    .settings-content { padding: 16px; }
    .pr-tabs { padding: 0 12px; gap: 4px; }
  }
  @media (max-width: 360px) { .pr-tabs { padding-inline: 8px; gap: 0; } }
</style>
