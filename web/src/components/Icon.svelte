<script lang="ts">
  let { name, size = 18 }: { name: 'skim' | 'pr' | 'chat' | 'verify' | 'settings' | 'refresh' | 'back'; size?: number } = $props();
  const paths = {
    skim: 'M4 5h16M4 12h12M4 19h8',
    pr: 'M6 8v8m12-8v5a5 5 0 0 1-5 5h-2m4-15 3 3 3-3',
    chat: 'M21 11.5a8.5 8.5 0 0 1-8.5 8.5H4l-3 2 2-6a8.5 8.5 0 1 1 18-4.5ZM7 9h10M7 13h6',
    verify: 'm9 11 3 3L22 4M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11',
    settings: 'm9.5 3-.7 2.1-1.5.9-2.2-.5-2.5 4.3 1.5 1.6v1.7l-1.5 1.6 2.5 4.3 2.2-.5 1.5.9.7 2.1h5l.7-2.1 1.5-.9 2.2.5 2.5-4.3-1.5-1.6v-1.7l1.5-1.6-2.5-4.3-2.2.5-1.5-.9-.7-2.1Z',
    refresh: 'M20 7v5h-5M4 17v-5h5M6.1 6.1A8 8 0 0 1 19.5 9M4.5 15a8 8 0 0 0 13.4 2.9',
    back: 'm12 5-7 7 7 7M5 12h15',
  };
</script>

<svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
  <path d={paths[name]} />
  {#if name === 'pr'}<circle cx="6" cy="5" r="3" /><circle cx="6" cy="19" r="3" />{/if}
  {#if name === 'settings'}<circle cx="12" cy="12" r="3" />{/if}
</svg>

<style>
  svg { display: inline-block; flex: 0 0 auto; vertical-align: middle; }
</style>
