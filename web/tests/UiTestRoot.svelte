<script lang="ts">
  import type { Component } from 'svelte';
  import * as Tooltip from '../src/lib/components/ui/tooltip/index.js';

  let { component: ComponentUnderTest, componentProps }: {
    component: Component<any>;
    componentProps: Record<string, unknown>;
  } = $props();
</script>

<Tooltip.Provider>
  <ComponentUnderTest {...componentProps} />
</Tooltip.Provider>
