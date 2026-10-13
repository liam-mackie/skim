<script lang="ts">
  import { Button, type ButtonProps } from '$lib/components/ui/button/index.js';
  import * as Tooltip from '$lib/components/ui/tooltip/index.js';
  import { cn } from '$lib/utils';

  let {
    label,
    text,
    tooltip = label,
    variant = 'ghost',
    size,
    class: className,
    disabled = false,
    ref = $bindable<HTMLElement | null>(null),
    children,
    ...rest
  }: ButtonProps & { label: string; text?: string; tooltip?: string } = $props();

  const controlSize = $derived(size ?? (text ? 'default' : 'icon'));
  const active = $derived(rest['aria-selected'] === true || rest['aria-pressed'] === true);
  const controlClass = $derived(cn(className, active && 'bg-primary text-primary-foreground shadow-sm hover:bg-primary/90 hover:text-primary-foreground dark:hover:bg-primary/90 aria-expanded:bg-primary aria-expanded:text-primary-foreground'));

  // Native dialogs are in the top layer; body portals would sit behind their backdrop.
  let portalTarget = $state<HTMLDialogElement | undefined>();
  $effect(() => { portalTarget = ref?.closest('dialog') ?? undefined; });
</script>

<Tooltip.Root>
  <Tooltip.Trigger>
    {#snippet child({ props })}
      {#if disabled}
        <span {...props} class="inline-flex" role="button" aria-disabled="true" tabindex="0" aria-label={label}>
          <Button {...rest} bind:ref {variant} size={controlSize} class={controlClass} disabled aria-hidden="true" aria-label={label} tabindex={-1}>
            {@render children?.()}{#if text}<span>{text}</span>{/if}
          </Button>
        </span>
      {:else}
        <Button {...props} {...rest} bind:ref {variant} size={controlSize} class={controlClass} aria-label={label}>
          {@render children?.()}{#if text}<span>{text}</span>{/if}
        </Button>
      {/if}
    {/snippet}
  </Tooltip.Trigger>
  <Tooltip.Content portalProps={{ to: portalTarget }}>{tooltip}</Tooltip.Content>
</Tooltip.Root>
