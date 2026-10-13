import createDOMPurify from 'dompurify';
import { decodeHTML } from 'entities';
import { Marked, Renderer, type Tokens } from 'marked';

export type FileReference = { path: string; line?: number };
export type Markdown = { html: string; references: FileReference[] };

const escapeHTML = (text: string) => text.replace(/[&<>"']/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char]!);
const fileHref = (index: number) => `#skim-file-${index}`;

/** Only exact changed paths and positive, safe line numbers can navigate the diff. */
export function resolveFileReference(text: string, paths: ReadonlySet<string>): FileReference | null {
  if (paths.has(text)) return { path: text };
  const match = /^(.*?)(?::|#L)(\d+)$/.exec(text);
  if (!match || !paths.has(match[1]!)) return null;
  const line = Number(match[2]);
  return Number.isSafeInteger(line) && line > 0 ? { path: match[1]!, line } : null;
}

function externalHref(href: string): string | null {
  try {
    const url = new URL(decodeHTML(href));
    return ['https:', 'http:', 'mailto:'].includes(url.protocol) ? url.href : null;
  } catch {
    return null;
  }
}

/** Only absolute HTTPS images load; anything else falls back to text. */
function imageSource(src: string): string | null {
  try {
    const url = new URL(decodeHTML(src));
    return url.protocol === 'https:' && !url.username && !url.password ? url.href : null;
  } catch {
    return null;
  }
}

const dimension = (value: string | null) => (value && /^\d{1,4}%?$/.test(value.trim()) ? value.trim() : null);

class MarkdownRenderer extends Renderer {
  readonly references: FileReference[] = [];
  private linkDepth = 0;

  constructor(private readonly paths: ReadonlySet<string>, private readonly images: boolean, private readonly signed?: (src: string) => string | undefined) {
    super();
  }

  /** Escaped attributes only; the sanitizer rechecks the result. A signed URL is displayed, the original stays the link. */
  private imageTag(original: string, alt: string, title: string | null, width: string | null = null, height: string | null = null): string {
    const signed = this.signed?.(original);
    const src = (signed && imageSource(signed)) || original;
    const attrs = [`src="${escapeHTML(src)}"`, `alt="${escapeHTML(alt)}"`, 'loading="lazy"', 'referrerpolicy="no-referrer"'];
    if (title) attrs.push(`title="${escapeHTML(title)}"`);
    if (width) attrs.push(`width="${width}"`);
    if (height) attrs.push(`height="${height}"`);
    const img = `<img ${attrs.join(' ')}>`;
    // Like GitHub, a standalone image opens full size; an image inside a link keeps that link.
    return this.linkDepth ? img : `<a href="${escapeHTML(original)}" target="_blank" rel="noopener noreferrer">${img}</a>`;
  }

  /** GitHub attachments arrive as raw `<img>` tags, often wrapped in `<picture>`; only the img survives. */
  private rawImages(html: string): string {
    let result = '';
    let end = 0;
    for (const match of html.matchAll(/<img\b[^>]*>|<\/?picture\b[^>]*>|<source\b[^>]*>/gi)) {
      result += escapeHTML(html.slice(end, match.index));
      end = match.index + match[0].length;
      if (!/^<img/i.test(match[0])) continue;
      const template = document.createElement('template');
      template.innerHTML = match[0];
      const img = template.content.querySelector('img');
      const src = img && imageSource(img.getAttribute('src') ?? '');
      result += src ? this.imageTag(src, img.getAttribute('alt') ?? 'Image', img.getAttribute('title'), dimension(img.getAttribute('width')), dimension(img.getAttribute('height'))) : escapeHTML(match[0]);
    }
    return result + escapeHTML(html.slice(end));
  }

  private fileLink(reference: FileReference, label: string): string {
    const index = this.references.push(reference) - 1;
    return `<a href="${fileHref(index)}" data-file-ref="${index}" title="Open in diff">${label}</a>`;
  }

  /** HTML comments (e.g. PR template hints) are hidden like GitHub does; other raw HTML stays literal text. An unclosed comment hides the rest, as in HTML. */
  override html({ text }: Tokens.HTML | Tokens.Tag): string {
    const visible = text.replace(/<!--[\s\S]*?(?:-->|$)/g, '');
    return this.images ? this.rawImages(visible) : escapeHTML(visible);
  }

  override code(token: Tokens.Code): string {
    if (token.lang !== 'suggestion') return super.code(token);
    return `<div class="code-suggestion"><div class="suggestion-label">Suggested change</div>${super.code(token)}${token.text === '' ? '<p class="suggestion-deletion">Delete the selected lines.</p>' : ''}</div>`;
  }

  override codespan(token: Tokens.Codespan): string {
    const code = super.codespan(token);
    const reference = this.linkDepth === 0 && resolveFileReference(token.text, this.paths);
    return reference ? this.fileLink(reference, code) : code;
  }

  override text(token: Tokens.Text | Tokens.Escape): string {
    if (this.linkDepth || ('tokens' in token && token.tokens) || !this.paths.size) return super.text(token);
    // This recognises file references only; all Markdown parsing belongs to marked.
    const pattern = /[\w@./-]+\.[\w]+(?:(?::[+-]?|#L)\d+)?/g;
    let result = '';
    let end = 0;
    for (const match of token.text.matchAll(pattern)) {
      const reference = resolveFileReference(match[0], this.paths);
      if (!reference) continue;
      result += super.text({ ...token, text: token.text.slice(end, match.index) });
      result += this.fileLink(reference, super.text({ ...token, text: match[0] }));
      end = match.index + match[0].length;
    }
    return result + super.text({ ...token, text: token.text.slice(end) });
  }

  override link(token: Tokens.Link): string {
    this.linkDepth++;
    const label = this.parser.parseInline(token.tokens);
    this.linkDepth--;
    const destination = decodeHTML(token.href);
    let reference = resolveFileReference(destination, this.paths);
    if (!reference) {
      try { reference = resolveFileReference(decodeURIComponent(destination), this.paths); } catch { /* Invalid encoding is not a file reference. */ }
    }
    if (reference) return this.fileLink(reference, label);
    const href = externalHref(token.href);
    if (!href) return label;
    const title = token.title ? ` title="${escapeHTML(decodeHTML(token.title))}"` : '';
    return `<a href="${escapeHTML(href)}" target="_blank" rel="noopener noreferrer"${title}>${label}</a>`;
  }

  override image(token: Tokens.Image): string {
    const alt = decodeHTML(token.text || 'Image');
    const src = this.images ? imageSource(token.href) : null;
    if (src) return this.imageTag(src, alt, token.title ? decodeHTML(token.title) : null);
    const label = escapeHTML(alt);
    const href = externalHref(token.href);
    // Model-generated content never emits img/src: an image URL could exfiltrate data in a background request.
    return href
      ? `<a href="${escapeHTML(href)}" target="_blank" rel="noopener noreferrer">${label} (image link)</a>`
      : `${label} (image omitted)`;
  }

  override checkbox({ checked }: Tokens.Checkbox): string {
    return `<input type="checkbox" disabled${checked ? ' checked' : ''} aria-label="${checked ? 'Completed' : 'Incomplete'} task"> `;
  }

  override table(token: Tokens.Table): string {
    return `<div class="markdown-table" tabindex="0" role="region" aria-label="Markdown table">${super.table(token)}</div>`;
  }
}

// A separate instance keeps this boundary independent of any other sanitizer users.
const sanitizer = createDOMPurify();
sanitizer.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'INPUT') {
    node.setAttribute('type', 'checkbox');
    node.setAttribute('disabled', '');
  }
  if (node.tagName === 'IMG') {
    if (!imageSource(node.getAttribute('src') ?? '')) node.remove();
    else {
      node.setAttribute('loading', 'lazy');
      node.setAttribute('referrerpolicy', 'no-referrer');
    }
  }
});

/**
 * `images` loads remote HTTPS images. Enable it only for human-authored text (PR descriptions,
 * GitHub discussions); model output keeps images as links so a URL cannot trigger a request.
 * `signed` swaps a GitHub attachment URL for its pre-signed form so private-repository images load.
 */
export function renderMarkdown(text: string, knownPaths: readonly string[], { images = false, signed }: { images?: boolean; signed?: (src: string) => string | undefined } = {}): Markdown {
  const renderer = new MarkdownRenderer(new Set(knownPaths), images, signed);
  const parser = new Marked({ gfm: true, breaks: false });
  parser.setOptions({ renderer });
  const html = parser.parse(text, { async: false });
  const imageTags = images ? ['img'] : [];
  const imageAttrs = images ? ['src', 'alt', 'width', 'height', 'loading', 'referrerpolicy'] : [];
  return {
    html: sanitizer.sanitize(html, {
      ALLOWED_TAGS: ['p', 'br', 'hr', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'strong', 'em', 'del', 'blockquote', 'ul', 'ol', 'li', 'a', 'code', 'pre', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'div', 'input', ...imageTags],
      ALLOWED_ATTR: ['href', 'title', 'target', 'rel', 'class', 'start', 'align', 'type', 'disabled', 'checked', 'tabindex', 'role', 'aria-label', 'data-file-ref', ...imageAttrs],
      // These attributes are not URLs; only href and src should use the protocol allowlist.
      ADD_URI_SAFE_ATTR: ['target', 'rel', 'start', 'align', 'type', 'disabled', 'checked', 'tabindex', 'aria-label', 'data-file-ref', 'alt', 'width', 'height', 'loading', 'referrerpolicy'],
      ALLOW_DATA_ATTR: false,
      ALLOW_ARIA_ATTR: false,
      ALLOWED_URI_REGEXP: /^(?:https?:\/\/|mailto:|#skim-file-\d+$)/i,
    }),
    references: renderer.references,
  };
}

type FileLinkOptions = {
  references: readonly FileReference[];
  paths: readonly string[];
  selectFile: (reference: FileReference) => void;
};

/** Native anchors provide keyboard activation; the action revalidates current PR membership. */
export function markdownFileLinks(node: HTMLElement, initial: FileLinkOptions) {
  let options = initial;
  const click = (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const target = event.target instanceof Element ? event.target.closest('a[data-file-ref]') : null;
    if (!target || !node.contains(target)) return;
    event.preventDefault();
    const value = target.getAttribute('data-file-ref') ?? '';
    if (!/^\d+$/.test(value)) return;
    const index = Number(value);
    const reference = options.references[index];
    if (!reference || target.getAttribute('href') !== fileHref(index) || !options.paths.includes(reference.path)) return;
    if (reference.line !== undefined && (!Number.isSafeInteger(reference.line) || reference.line <= 0)) return;
    options.selectFile(reference);
  };
  node.addEventListener('click', click);
  return {
    update(next: FileLinkOptions) { options = next; },
    destroy() { node.removeEventListener('click', click); },
  };
}
