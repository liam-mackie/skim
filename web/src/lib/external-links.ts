import { api } from './api';

/**
 * Desktop web views do not open target="_blank" links. When the server reports a
 * desktop host, such http(s) links open in the system browser instead; in a
 * normal browser nothing changes.
 */
export async function installExternalLinks(): Promise<void> {
  let desktop = false;
  try {
    desktop = (await api.host()).desktop;
  } catch {
    return;
  }
  if (!desktop) return;
  const open = (event: MouseEvent) => {
    if (event.defaultPrevented || (event.type === 'auxclick' && event.button !== 1)) return;
    const link = event.target instanceof Element ? event.target.closest('a[href]') : null;
    if (!(link instanceof HTMLAnchorElement) || link.target !== '_blank') return;
    if (link.protocol !== 'https:' && link.protocol !== 'http:') return;
    event.preventDefault();
    api.openExternal(link.href).catch((e) => console.error('Could not open link in the browser', e));
  };
  document.addEventListener('click', open);
  document.addEventListener('auxclick', open);
}
