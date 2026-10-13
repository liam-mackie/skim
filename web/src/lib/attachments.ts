const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * Asset UUID of a GitHub image attachment URL, matching the server's provider.AttachmentID:
 * github.com/user-attachments/assets/UUID, legacy user-images, or signed private-user-images.
 */
export function attachmentId(raw: string): string | null {
  let url: URL;
  try { url = new URL(raw); } catch { return null; }
  if (url.protocol !== 'https:' || url.username || url.password) return null;
  let candidate = '';
  if (url.host === 'github.com') {
    const rest = url.pathname.startsWith('/user-attachments/assets/') ? url.pathname.slice('/user-attachments/assets/'.length) : '';
    if (rest.includes('/')) return null;
    candidate = rest;
  } else if (url.host === 'user-images.githubusercontent.com' || url.host === 'private-user-images.githubusercontent.com') {
    const base = url.pathname.slice(url.pathname.lastIndexOf('/') + 1).replace(/\.[^.]*$/, '');
    candidate = base.slice(base.indexOf('-') + 1);
    if (!base.includes('-')) return null;
  }
  return UUID.test(candidate) ? candidate.toLowerCase() : null;
}
