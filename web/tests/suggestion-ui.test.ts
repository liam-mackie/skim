import assert from 'node:assert/strict';
import test from 'node:test';
import { JSDOM } from 'jsdom';
import { findSuggestion } from '../src/lib/suggestion';

const dom = new JSDOM('<!doctype html><body></body>', { url: 'http://localhost/', pretendToBeVisual: true });
for (const key of ['window', 'document', 'navigator', 'Element', 'HTMLElement', 'HTMLMediaElement', 'Node', 'Text', 'Comment', 'Event', 'CustomEvent', 'MouseEvent', 'HTMLInputElement', 'HTMLTextAreaElement', 'HTMLSelectElement', 'MutationObserver', 'DOMException']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: (dom.window as unknown as Record<string, unknown>)[key] });
}
// Browser component modules must initialize after installing the DOM, not at static module evaluation.
const { mount, unmount, flushSync, tick } = await import('./mount-ui');
const { default: CommentEditor } = await import('../src/components/CommentEditor.svelte');

async function input(element: HTMLTextAreaElement, value: string) {
  element.value = value;
  element.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  await tick();
}

test('replacement editor safely rewrites fences, previews literal code and supports deleting the range', async (t) => {
  const target = document.createElement('div'); document.body.append(target);
  const editor = mount(CommentEditor, { target, props: { inputId: 'suggestion-body', body: 'Please preserve the contract.', selection: { code: '\treturn oldValue;', reason: null } } });
  t.after(async () => { await unmount(editor); target.remove(); });
  flushSync();
  target.querySelector<HTMLButtonElement>('button[aria-label="Insert code suggestion"]')!.click(); flushSync(); await tick();
  const replacement = target.querySelector<HTMLTextAreaElement>('#suggestion-body-replacement')!;
  const raw = target.querySelector<HTMLTextAreaElement>('#suggestion-body')!;
  assert.equal(replacement.value, '\treturn oldValue;');
  const code = '\tconst text = "```";\n<script>window.untrusted = true</script>\n';
  await input(replacement, code);
  assert.equal(findSuggestion(raw.value)?.code, code);
  assert.ok(raw.value.startsWith('Please preserve the contract.\n\n'));
  target.querySelector<HTMLButtonElement>('button[aria-controls$="-preview"]')!.click(); flushSync();
  const preview = target.querySelector('.comment-preview')!;
  assert.equal(preview.querySelector('script'), null);
  assert.match(preview.querySelector('pre code')!.textContent!, /<script>window.untrusted = true<\/script>/);
  target.querySelector<HTMLButtonElement>('button[aria-controls$="-preview"]')!.click(); flushSync();
  await input(replacement, '');
  target.querySelector<HTMLButtonElement>('button[aria-controls$="-preview"]')!.click(); flushSync();
  assert.equal(findSuggestion(raw.value)?.code, '');
  assert.ok(preview.querySelector('.suggestion-deletion'), 'an empty replacement is visibly a deletion, not lost content');
  target.querySelector<HTMLButtonElement>('button[aria-controls$="-preview"]')!.click(); flushSync();
  target.querySelector<HTMLButtonElement>('.replacement-editor button')!.click(); flushSync(); await tick();
  assert.equal(raw.readOnly, false);
  assert.equal(target.querySelector('.replacement-editor'), null);
});

test('unsupported anchors disable code insertion without preventing ordinary comments or preview', async (t) => {
  const target = document.createElement('div'); document.body.append(target);
  const editor = mount(CommentEditor, { target, props: { inputId: 'ordinary-body', body: '', selection: { code: null, reason: 'Select current new-side code instead.' } } });
  t.after(async () => { await unmount(editor); target.remove(); });
  flushSync();
  assert.equal(target.querySelector<HTMLButtonElement>('button[aria-label="Insert code suggestion"]')!.disabled, true);
  const raw = target.querySelector<HTMLTextAreaElement>('#ordinary-body')!;
  assert.equal(raw.readOnly, false);
  await input(raw, 'A plain review comment still works.');
  target.querySelector<HTMLButtonElement>('button[aria-controls$="-preview"]')!.click(); flushSync();
  assert.match(target.querySelector('.comment-preview')!.textContent!, /A plain review comment still works/);
});
