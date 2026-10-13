<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import { unitElementId } from '../lib/tree';
  import { Checkbox } from '$lib/components/ui/checkbox/index.js';
  import * as Tooltip from '$lib/components/ui/tooltip/index.js';
  import LoaderCircle from '@lucide/svelte/icons/loader-circle';

  let { paths, label = 'Reviewed', ariaLabel }: { paths: readonly string[]; label?: string; ariaLabel?: string } = $props();
  const count = $derived(paths.reduce((total, path) => total + Number(store.reviewedPaths.has(path)), 0));
  const checked = $derived(paths.length > 0 && count === paths.length);
  const mixed = $derived(count > 0 && count < paths.length);
  const pending = $derived(paths.some((path) => store.reviewPending[path]));
  const disabled = $derived(!paths.length || store.booting || pending);
  const id = $props.id();
  const reason = $derived(pending ? 'Saving review progress' : store.booting ? 'Loading review progress' : !paths.length ? 'No files to mark reviewed' : 'Local reading progress; not a submitted review');
  let input = $state<HTMLButtonElement | null>(null);

  async function change(requested: boolean) {
    const checkbox = input;
    if (!checkbox) return;
    const hadFocus = document.activeElement === checkbox;
    const filePath = checkbox.closest<HTMLElement>('[data-file-path]')?.dataset.filePath;
    if (disabled) return;
    await store.setReviewed(paths, requested);
    await tick();
    // Saving may blur the checkbox. Restore focus to this control, or the unit
    // control when completing its final file hides the file header.
    if (!hadFocus || !checkbox.isConnected || (document.activeElement !== document.body && document.activeElement !== checkbox)) return;
    if (checkbox.getClientRects().length) checkbox.focus({ preventScroll: true });
    else if (filePath) {
      const unit = store.state?.groups.find((group) => group.paths.includes(filePath));
      const control = unit && document.getElementById(unitElementId(unit.id))?.querySelector<HTMLButtonElement>('.reviewed-control [role="checkbox"]');
      if (control?.getClientRects().length) control.focus({ preventScroll: true });
    }
  }
</script>

<Tooltip.Root>
  <Tooltip.Trigger>
    {#snippet child({ props })}
      <span class={['reviewed-control inline-flex min-h-7 shrink-0 items-center gap-1.5 whitespace-nowrap text-xs', checked ? 'text-foreground' : 'text-muted-foreground', pending && 'cursor-wait']} aria-busy={pending}>
        <!-- Controlled bindings keep the saved state visible until the server responds. -->
        <Checkbox {...props} {id} bind:ref={input} bind:checked={() => checked, change} bind:indeterminate={() => mixed, () => {}} {disabled} aria-label={ariaLabel ?? label} aria-describedby={`${id}-reason`} />
        <label for={id} class="m-0 cursor-pointer text-xs font-medium">{label}</label>
        {#if pending}<LoaderCircle class="size-3 animate-spin" aria-hidden="true" />{/if}
        <span id={`${id}-reason`} class="sr-only" role={pending ? 'status' : undefined}>{reason}</span>
      </span>
    {/snippet}
  </Tooltip.Trigger>
  <Tooltip.Content>{reason}</Tooltip.Content>
</Tooltip.Root>
