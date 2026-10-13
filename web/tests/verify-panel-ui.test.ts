import assert from 'node:assert/strict';
import test from 'node:test';
import type { TestContext } from 'node:test';
import { JSDOM } from 'jsdom';
import type { AppState, ChatThread, Permission, VerificationJob } from '../src/lib/api';

const dom = new JSDOM('<!doctype html><body></body>', { url: 'http://localhost/', pretendToBeVisual: true });
for (const key of ['window', 'document', 'navigator', 'Element', 'HTMLElement', 'HTMLMediaElement', 'Node', 'Text', 'Comment', 'Event', 'CustomEvent', 'MouseEvent', 'HTMLInputElement', 'HTMLTextAreaElement', 'HTMLSelectElement', 'MutationObserver', 'DOMException']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: (dom.window as unknown as Record<string, unknown>)[key] });
}
// Svelte's browser runtime must initialize after the DOM exists.
const { mount, unmount, flushSync, tick } = await import('./mount-ui');
// Browser modules must initialize after the DOM is installed; static imports would cross that loading boundary.
const { store } = await import('../src/lib/store.svelte.ts');
const { default: VerifyPanel } = await import('../src/components/VerifyPanel.svelte');
const identity = { prKey: 'github/team/repo/1', baseSha: 'base', headSha: 'head' };
function job(id: string, status: VerificationJob['status'] = 'complete'): VerificationJob {
  return { ...identity, id: `job-${id}`, threadId: id, prUrl: 'https://github.com/team/repo/pull/1', path: `${id}.ts`, side: 'RIGHT', startLine: 1, endLine: 2, question: `Question ${id}`, status, revision: 1, startedAt: '2026-01-01', updatedAt: '2026-01-01' };
}
function thread(id: string): ChatThread {
  return { id, revision: 1, kind: 'verification', headSha: 'head', path: `${id}.ts`, side: 'RIGHT', startLine: 1, endLine: 2, messages: [{ role: 'user', content: `Question ${id}` }, { role: 'assistant', content: `**Report ${id}**` }] };
}
function setup(t: TestContext) {
  store.state = {
    revision: 1,
    pullRequest: { provider: 'github', owner: 'team', repo: 'repo', number: 1, title: 'Review', body: '', url: 'https://github.com/team/repo/pull/1', author: 'author', baseSha: 'base', headSha: 'head', baseRef: 'main', headRef: 'feature' },
    files: ['one.ts', 'two.ts'].map((path) => ({ path, status: 'modified', additions: 2, deletions: 1, patchStatus: 'available', patch: '@@ -1 +1,2 @@\n-old\n+new\n+line' })), groups: [], comments: [], chats: [thread('one'), thread('two')], style: '',
    settings: { githubTokenEnv: 'GITHUB_TOKEN', providers: [], roles: { chat: { provider: '', model: '' }, review: { provider: '', model: '' }, jev: { provider: '', model: '' } }, acp: { command: 'agent', args: [], managedRoot: '/isolated' } },
    workspace: { status: 'ready', root: '/isolated', path: '/isolated/pr', id: 'workspace' },
  } satisfies AppState;
  store.booting = false;
  store.activeVerifyId = null;
  store.conversationDrafts = {};
  store.verificationJobs = [job('one'), job('two')];
  store.permissions = [];
  store.permissionErrors = {};
  store.permissionSubmitting = {};
  store.verificationErrors = {};
  store.verificationThreadErrors = {};
  store.verificationCancelling = {};
  store.verificationPollingError = null;
  store.activityOpen = false;
  const target = document.createElement('div'); document.body.append(target);
  const component = mount(VerifyPanel, { target });
  t.after(async () => { await unmount(component); target.remove(); });
  flushSync();
  return target;
}
function button(target: Element, label: string) {
  const found = [...target.querySelectorAll<HTMLButtonElement>('button')].find((item) => item.getAttribute('aria-label') === label || item.textContent?.includes(label));
  assert.ok(found, `Button ${label} exists`);
  return found;
}
async function click(target: Element, label: string) {
  button(target, label).click(); flushSync(); await tick();
}
async function draft(target: Element, text: string) {
  const input = target.querySelector('textarea')!;
  input.value = text; input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync(); await tick();
}

test('checks navigate to one thread, preserve per-thread drafts and stay put on background completion', async (t) => {
  const target = setup(t);
  assert.equal(target.querySelector('textarea'), null);
  await click(target, 'Question one');
  assert.equal(target.querySelector('.threads'), null);
  assert.match(target.querySelector('.conversation-list strong:not(.speaker)')!.textContent!, /Report one/);
  assert.equal(target.querySelector<HTMLDetailsElement>('.technical-details')!.open, false);
  await draft(target, 'unsent one');
  await click(target, 'Back to checks');
  assert.equal(target.querySelector('.conversation-list'), null);
  store.verificationJobs = [job('one'), { ...job('two'), revision: 2 }];
  store.state!.chats = [thread('one'), { ...thread('two'), revision: 2 }];
  flushSync();
  assert.equal(store.activeVerifyId, null);
  assert.equal(target.querySelector('textarea'), null);
  await click(target, 'Question two');
  assert.equal(target.querySelector('textarea')!.value, '');
  await draft(target, 'unsent two');
  await click(target, 'Back to checks');
  await click(target, 'Question one');
  assert.equal(target.querySelector('textarea')!.value, 'unsent one');
  store.verificationJobs = [job('one'), { ...job('two'), revision: 3 }];
  flushSync();
  assert.equal(store.activeVerifyId, 'one');
  assert.equal(target.querySelector('textarea')!.value, 'unsent one');
  await click(target, 'Activity');
  assert.equal(store.activityOpen, true);
});

test('active threads retain scoped consent details, cancellation and errors without blocking navigation', async (t) => {
  const target = setup(t);
  store.verificationJobs = [job('one', 'running'), job('two')];
  const permission: Permission = { id: 'permission-one', sessionId: 'session', jobId: 'job-one', threadId: 'one', prKey: identity.prKey, title: 'Run command', details: 'npm test -- --runInBand', options: [{ optionId: 'yes', name: 'Allow once', kind: 'allow_once' }, { optionId: 'no', name: 'Deny', kind: 'reject_once' }] };
  store.permissions = [permission, { ...permission, id: 'permission-two', threadId: 'two', jobId: 'job-two', title: 'Other command' }, { ...permission, id: 'other-pr', prKey: 'github/other/repo/2', title: 'Other PR command' }];
  store.permissionErrors[permission.id] = 'Permission response failed';
  store.verificationErrors['job-one'] = 'Cancellation failed';
  await click(target, 'Question one');
  await draft(target, 'continue later');
  assert.equal(button(target, 'Send follow-up').disabled, true);
  assert.equal(target.querySelectorAll('[data-permission-id]').length, 1);
  assert.match(target.querySelector('[aria-label="Action details"]')!.textContent!, /npm test -- --runInBand/);
  assert.equal(target.querySelector('[role="dialog"]'), null);
  assert.match(target.textContent!, /Permission response failed/);
  assert.match(target.textContent!, /Cancellation failed/);
  store.permissionSubmitting[permission.id] = true; flushSync();
  assert.equal(button(target, 'Allow once').disabled, true);
  assert.equal(button(target, 'Deny').disabled, true);
  store.verificationJobs = [job('one', 'cancelling'), job('two')]; flushSync();
  assert.equal(button(target, 'Cancelling').disabled, true);
  await click(target, 'Back to checks');
  await click(target, 'Question two');
  assert.equal(target.querySelector('textarea')!.value, '');
  assert.equal(target.querySelectorAll('[data-permission-id]').length, 1);
});

test('finished and failed threads retain reports, follow-ups, code access and stale safeguards', async (t) => {
  const target = setup(t);
  await click(target, 'Question one');
  await click(target, 'one.ts:1');
  assert.deepEqual(store.fileNavigation?.line, { side: 'RIGHT', line: 2 });
  assert.equal(store.fileNavigation?.path, 'one.ts');
  assert.equal([...target.querySelectorAll('button')].some((item) => item.textContent?.includes('Cancel check')), false);
  await draft(target, 'follow up');
  assert.equal(button(target, 'Send follow-up').disabled, false);
  store.verificationJobs = [{ ...job('one', 'failed'), error: 'Agent disconnected', suggestionsError: 'Parsing failed' }, job('two')];
  store.verificationThreadErrors.one = 'Follow-up rejected';
  store.verificationPollingError = 'Updates unavailable';
  flushSync();
  for (const message of ['Agent disconnected', 'Parsing failed', 'Follow-up rejected', 'Updates unavailable', 'Report one']) assert.match(target.textContent!, new RegExp(message));
  store.state!.chats = [{ ...thread('one'), stale: true }, thread('two')]; flushSync();
  assert.equal(button(target, 'Send follow-up').disabled, true);
  await click(target, 'Back to checks');
  assert.equal(target.querySelector('textarea'), null);
  assert.match(target.textContent!, /Updates unavailable/);
});
