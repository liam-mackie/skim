import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { JSDOM } from 'jsdom';
import { attachmentId } from '../src/lib/attachments';

const dom = new JSDOM('<!doctype html><body></body>', { url: 'http://localhost/' });
Object.assign(globalThis, { window: dom.window, document: dom.window.document, Element: dom.window.Element });
// Exercise the browser-only module initialization after installing the test DOM.
const { renderMarkdown, markdownFileLinks } = await import('../src/lib/markdown');
const fixture = readFileSync(new URL('./fixtures/markdown.txt', import.meta.url), 'utf8');

function render(text = fixture, paths = ['src/calc.go']) {
  const markdown = renderMarkdown(text, paths);
  const root = document.createElement('div');
  root.innerHTML = markdown.html;
  return { root, markdown };
}

test('GFM produces semantic nested blocks and preserves code and escaping', () => {
  const { root } = render();
  assert.equal(root.querySelector('h1')?.textContent, 'Review notes');
  assert.equal(root.querySelector('h2')?.textContent, 'Nested changes');
  assert.match(root.querySelector(':scope > p + p')!.textContent!, /^A second paragraph/);
  assert.equal(root.querySelector('p strong')?.textContent, 'strong');
  assert.equal(root.querySelector('p em')?.textContent, 'emphasis');
  assert.equal(root.querySelector('p del')?.textContent, 'obsolete');
  assert.equal(root.querySelector('p code')?.textContent, 'a < b && c > d');
  assert.match(root.querySelector('p')!.textContent!, /soft\nline break/);
  assert.equal(root.querySelector('ol > li > ul > li > ul > li strong')?.textContent, 'detail');
  assert.equal(root.querySelectorAll('blockquote p').length, 2);
  assert.equal(root.querySelectorAll('table thead th').length, 3);
  assert.equal(root.querySelectorAll('table tbody tr').length, 2);
  assert.equal(root.querySelector('th:nth-child(3)')?.getAttribute('align'), 'right');
  assert.equal(root.querySelector('.markdown-table')?.getAttribute('tabindex'), '0');
  assert.match(root.querySelector('pre code.language-html')!.textContent!, /^<script>window.__markdownXss = 'fenced';<\/script>\n<img src=/);
  assert.match(root.querySelector('pre code')!.textContent!, /const significant = "< > & \\" '";/);
  assert.equal(root.querySelectorAll('pre code script, pre code img').length, 0);
  assert.match(root.textContent!, /Escaped \*literal asterisks\*, & entity, and <not-a-real-tag> literal HTML\./);
  const checkboxes = [...root.querySelectorAll('input')];
  assert.equal(checkboxes.length, 2);
  assert.ok(checkboxes.every((input) => input.type === 'checkbox' && input.disabled));
  assert.equal(checkboxes[0]!.checked, true);
  assert.equal(checkboxes[1]!.checked, false);
});

test('HTML comments are hidden while other raw HTML and code stay literal', () => {
  const { root } = render('<!-- Describe your change -->\nKept <!-- inline hint --> text <b>bold?</b>\n\n<!--\nmulti-line\ntemplate\n-->\n\nAfter `<!-- code -->`\n\n```\n<!-- fenced -->\n```');
  assert.doesNotMatch(root.innerHTML, /Describe your change|inline hint|multi-line|template/);
  assert.match(root.textContent!, /Kept\s+text <b>bold\?<\/b>/);
  assert.equal(root.querySelector('b'), null);
  assert.equal(root.querySelector('p code')?.textContent, '<!-- code -->');
  assert.equal(root.querySelector('pre code')?.textContent, '<!-- fenced -->\n');
});

test('author images load over HTTPS only, including GitHub <img> attachments, and stay off for model text', () => {
  const text = [
    '![diagram &amp; flow](https://example.invalid/d.png "Flow")',
    '<img width="600" height="200" alt="Screenshot" src="https://github.com/user-attachments/assets/abc" onerror="window.__markdownXss=1">',
    '<picture><source media="(prefers-color-scheme: dark)" srcset="https://example.invalid/dark.png"><img alt="Logo" src="https://example.invalid/logo.png"></picture>',
    '[![badge](https://example.invalid/badge.svg)](https://example.invalid/ci)',
    '![plain](http://example.invalid/insecure.png) ![js](javascript:alert(1)) <img src="data:image/png;base64,AAAA" alt="inline"> <img width="100px; position:fixed" src="https://example.invalid/w.png" alt="w">',
    '<div>raw block</div>',
  ].join('\n\n');
  const root = document.createElement('div');
  root.innerHTML = renderMarkdown(text, [], { images: true }).html;
  const images = [...root.querySelectorAll('img')];
  assert.deepEqual(images.map((img) => img.getAttribute('src')), ['https://example.invalid/d.png', 'https://github.com/user-attachments/assets/abc', 'https://example.invalid/logo.png', 'https://example.invalid/badge.svg', 'https://example.invalid/w.png']);
  assert.ok(images.every((img) => img.getAttribute('loading') === 'lazy' && img.getAttribute('referrerpolicy') === 'no-referrer' && !img.hasAttribute('onerror') && !img.hasAttribute('style')));
  assert.equal(images[0]!.alt, 'diagram & flow');
  assert.equal(images[0]!.title, 'Flow');
  assert.equal(images[1]!.getAttribute('width'), '600');
  assert.equal(images[4]!.hasAttribute('width'), false, 'non-numeric dimensions are dropped');
  assert.equal(images[0]!.closest('a')?.getAttribute('href'), 'https://example.invalid/d.png', 'standalone images open full size');
  assert.equal(images[3]!.closest('a')?.getAttribute('href'), 'https://example.invalid/ci', 'linked badges keep their link');
  assert.equal(root.querySelectorAll('a a, picture, source, script, div').length, 0);
  assert.match(root.textContent!, /plain \(image link\)/);
  assert.match(root.textContent!, /js \(image omitted\)/);
  assert.match(root.textContent!, /<img src="data:image\/png;base64,AAAA" alt="inline">/);
  assert.match(root.textContent!, /<div>raw block<\/div>/);
  assert.equal((dom.window as unknown as Record<string, unknown>).__markdownXss, undefined);

  const model = document.createElement('div');
  model.innerHTML = renderMarkdown(text, []).html;
  assert.equal(model.querySelectorAll('img, [src]').length, 0, 'images stay off unless explicitly enabled');
});

test('GitHub attachments display their signed URL while links keep the original', () => {
  const uuid = '0d1e2f3a-4b5c-4d6e-8f90-a1b2c3d4e5f6';
  const original = `https://github.com/user-attachments/assets/${uuid}`;
  const signedUrl = `https://private-user-images.githubusercontent.com/42/900-${uuid}.png?jwt=abc`;
  assert.equal(attachmentId(original), uuid);
  assert.equal(attachmentId(signedUrl), uuid);
  assert.equal(attachmentId(`https://user-images.githubusercontent.com/42/900-${uuid.toUpperCase()}.png`), uuid);
  for (const other of [`http://github.com/user-attachments/assets/${uuid}`, `https://evil.example/user-attachments/assets/${uuid}`, 'https://github.com/user-attachments/files/1/a.pdf', `https://github.com/user-attachments/assets/${uuid}/x`]) {
    assert.equal(attachmentId(other), null, other);
  }
  const signed = (src: string) => (attachmentId(src) === uuid ? signedUrl : src.includes('unsafe') ? 'javascript:alert(1)' : undefined);
  const root = document.createElement('div');
  root.innerHTML = renderMarkdown(`![shot](${original})\n\n<img alt="raw" src="${original}">\n\n[![linked](${original})](https://example.invalid/ci)\n\n![unsafe](https://example.invalid/unsafe.png)`, [], { images: true, signed }).html;
  const images = [...root.querySelectorAll('img')];
  assert.deepEqual(images.map((img) => img.getAttribute('src')), [signedUrl, signedUrl, signedUrl, 'https://example.invalid/unsafe.png']);
  assert.deepEqual(images.map((img) => img.closest('a')?.getAttribute('href')), [original, original, 'https://example.invalid/ci', 'https://example.invalid/unsafe.png']);
});

test('untrusted HTML, images and unsafe or encoded URL schemes are inert', () => {
  const { root } = render();
  assert.equal(root.querySelectorAll('img, script, svg, form, iframe, object, style, button').length, 0);
  assert.equal(root.querySelectorAll('[onclick], [onload], [onerror], [src], [action]').length, 0);
  assert.match(root.textContent!, /<form action="https:\/\/exfil.invalid\/auth"><input type="password"/);
  assert.match(root.textContent!, /forged navigation/);
  for (const label of ['unsafe-js', 'encoded-js', 'percent-js', 'mixed-js', 'tab-js', 'unsafe-vb', 'unsafe-data', 'unsafe-file', 'relative', 'protocol-relative']) {
    assert.ok(![...root.querySelectorAll('a')].some((a) => a.textContent === label), label);
  }
  const image = [...root.querySelectorAll('a')].find((a) => a.textContent === 'Remote diagram (image link)');
  assert.equal(image?.getAttribute('href'), 'https://exfil.invalid/image?private=source');
  assert.match(root.textContent!, /Unsafe image \(image omitted\)/);
  for (const link of root.querySelectorAll('a:not([data-file-ref])')) {
    assert.ok(['https:', 'http:', 'mailto:'].includes(new URL(link.href).protocol));
    assert.equal(link.target, '_blank');
    assert.equal(link.rel, 'noopener noreferrer');
  }
  assert.equal((dom.window as unknown as Record<string, unknown>).__markdownXss, undefined);
});

test('only known paths navigate with positive lines, with click-time revalidation and teardown', () => {
  const paths = ['src/calc.go', 'src/a "quote".go'];
  const { root, markdown } = render('`src/calc.go:2` [third](src/calc.go:3) src/calc.go `src/calc.go#L4` [encoded](src%2Fcalc.go%3A5) `src/a "quote".go:6` `unknown.go:1` `src/calc.go:0` src/calc.go:-1 `src/calc.go:9007199254740992`', paths);
  assert.deepEqual(markdown.references, [
    { path: 'src/calc.go', line: 2 }, { path: 'src/calc.go', line: 3 }, { path: 'src/calc.go' },
    { path: 'src/calc.go', line: 4 }, { path: 'src/calc.go', line: 5 }, { path: 'src/a "quote".go', line: 6 },
  ]);
  const selected: unknown[] = [];
  const action = markdownFileLinks(root, { references: markdown.references, paths, selectFile: (ref) => selected.push(ref) });
  const links = [...root.querySelectorAll('a')];
  assert.equal(links.length, 6);
  const click = (element: Element) => element.dispatchEvent(new dom.window.MouseEvent('click', { bubbles: true, cancelable: true }));
  click(links[0]!.querySelector('code')!);
  click(links[1]!);
  assert.deepEqual(selected, [{ path: 'src/calc.go', line: 2 }, { path: 'src/calc.go', line: 3 }]);
  action.update({ references: markdown.references, paths: [], selectFile: (ref) => selected.push(ref) });
  click(links[0]!);
  assert.equal(selected.length, 2, 'PR changes invalidate old file links');
  action.update({ references: markdown.references, paths, selectFile: (ref) => selected.push(ref) });
  links[0]!.setAttribute('href', '#different');
  click(links[0]!);
  links[1]!.setAttribute('data-file-ref', '999');
  click(links[1]!);
  assert.equal(selected.length, 2, 'tampered reference attributes cannot navigate');
  action.destroy();
  click(links[2]!);
  assert.equal(selected.length, 2);
});

test('file references inside link labels never create nested anchors', () => {
  const { root, markdown } = render('[`src/calc.go:2`](https://example.invalid/) [src/calc.go:3](src/calc.go:4)');
  assert.equal(root.querySelectorAll('a').length, 2);
  assert.equal(root.querySelectorAll('a a').length, 0);
  assert.deepEqual(markdown.references, [{ path: 'src/calc.go', line: 4 }]);
  assert.equal(root.querySelector('a code')?.textContent, 'src/calc.go:2');
});

test('entities are decoded once in links, titles and image labels without changing literal code', () => {
  const { root } = render('[a &amp; b](https://example.invalid/?a=1&amp;b=2 \"A &amp; B\") ![a &amp; b](https://example.invalid/image) `&amp; <b>`');
  assert.equal(root.querySelector('a')?.getAttribute('href'), 'https://example.invalid/?a=1&b=2');
  assert.equal(root.querySelector('a')?.title, 'A & B');
  assert.equal(root.querySelector('a')?.textContent, 'a & b');
  assert.equal(root.querySelectorAll('a')[1]?.textContent, 'a & b (image link)');
  assert.equal(root.querySelector('code')?.textContent, '&amp; <b>');
});
