import { mount as mountComponent, type Component } from 'svelte';
import UiTestRoot from './UiTestRoot.svelte';
export { unmount, flushSync, tick } from 'svelte';

// Match App's shared context when mounting a single UI surface.
export function mount(component: Component<any>, options: Parameters<typeof mountComponent>[1]) {
  return mountComponent(UiTestRoot, {
    ...options,
    props: { component, componentProps: options.props ?? {} },
  });
}
