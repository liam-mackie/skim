import assert from 'node:assert/strict';
import test, { type TestContext } from 'node:test';
import { JSDOM } from 'jsdom';
import type { Analysis, AppState, ChatThread, DeletedComment, FileReviewUpdate, Permission, ReviewComment, ReviewProgress, Transport, VerificationAccepted, VerificationJob, VerificationUpdate } from '../src/lib/api';
import { ApiError, isLiveComment, setTransport } from '../src/lib/api';
import { conflictReason, mergeEntities, mergeState, mergeVerificationJobs, type Busy } from '../src/lib/concurrency';
import { diffElementId } from '../src/lib/diff';

const dom = new JSDOM('<!doctype html><body></body>', { url: 'http://localhost/', pretendToBeVisual: true });
for (const key of ['window', 'document', 'navigator', 'Element', 'HTMLElement', 'HTMLMediaElement', 'Node', 'Text', 'Comment', 'Event', 'CustomEvent', 'MouseEvent', 'HTMLInputElement', 'HTMLTextAreaElement', 'HTMLSelectElement', 'MutationObserver', 'DOMException']) {
  Object.defineProperty(globalThis, key, { configurable: true, value: (dom.window as unknown as Record<string, unknown>)[key] });
}
// Browser modules must initialize after the test DOM exists, matching the application's module-loading boundary.
const { Store, store: mountedStore } = await import('../src/lib/store.svelte.ts');
const anchor = { path: 'src/code.ts', side: 'RIGHT' as const, start: 1, end: 2, hunk: 0 };
// Navigation also initializes Svelte browser modules; the DOM must exist before module evaluation.
const { ReviewNavigation, readReviewRoute } = await import('../src/lib/navigation.svelte.ts');
const identity = { prKey: 'github/team/repo/1', baseSha: 'base', headSha: 'head' };
const permission: Permission = { id: 'permission-1', sessionId: 'session', jobId: 'job-verified', threadId: 'verified', prKey: identity.prKey, prUrl: 'https://github.com/team/repo/pull/1', path: anchor.path, question: 'check this', title: 'Run checks?', options: [{ optionId: 'yes', name: 'Allow', kind: 'allow_once' }, { optionId: 'no', name: 'Deny', kind: 'reject_once' }] };
const finished: ReviewProgress = { id: 'review-1', status: 'complete', phase: 'finished', findings: 1, rejected: 0, startedAt: '2026-01-01T00:00:00Z', updatedAt: '2026-01-01T00:00:01Z', finishedAt: '2026-01-01T00:00:01Z' };

function comment(id: string, revision = 1, body = id): ReviewComment {
  return { id, revision, body, path: anchor.path, startLine: 1, endLine: 2, side: 'RIGHT', status: 'pending', source: 'manual', createdAt: '2026-01-01', headSha: 'head' };
}
function suggestion(id: string, source: 'review' | 'verification' = 'review'): ReviewComment {
  return { ...comment(id), source, status: 'suggested' };
}
function thread(id: string, revision = 1, kind: ChatThread['kind'] = 'global'): ChatThread {
  return { id, revision, kind, headSha: 'head', path: anchor.path, startLine: 1, endLine: 2, side: 'RIGHT', messages: [{ role: 'user', content: id }, { role: 'assistant', content: `answer ${revision}` }] };
}
function accepted(id: string, status: VerificationJob['status'] = 'queued', revision = 1): VerificationAccepted {
  const job: VerificationJob = {
    ...identity, id: `job-${id}`, threadId: id, prUrl: 'https://github.com/team/repo/pull/1', path: anchor.path,
    startLine: anchor.start, endLine: anchor.end, side: anchor.side, question: id, status, revision,
    startedAt: '2026-01-01T00:00:00Z', updatedAt: `2026-01-01T00:00:0${revision}Z`,
  };
  return { job, thread: { ...thread(id, revision, 'verification'), messages: status === 'queued' ? [{ role: 'user', content: id }] : thread(id, revision).messages, verification: job } };
}
function analysis(generation = 'generation-1', revision = 1): Analysis {
  return { key: identity.prKey, baseSha: 'base', headSha: 'head', generation, revision, risk: { status: 'complete', completed: 1, total: 1 }, grouping: { status: 'complete', completed: 1, total: 1 } };
}
function state(revision = 1, patch: Partial<AppState> = {}): AppState {
  return {
    revision,
    pullRequest: { provider: 'github', owner: 'team', repo: 'repo', number: 1, title: 'Review me', body: '', url: 'https://github.com/team/repo/pull/1', author: 'author', baseSha: 'base', headSha: 'head', baseRef: 'main', headRef: 'feature' },
    files: [{ path: anchor.path, status: 'modified', additions: 2, deletions: 1, patch: '@@ -1 +1,2 @@\n-old\n+new\n+line', patchStatus: 'available' }],
    groups: [], comments: [comment('editable')], chats: [thread('existing')], style: 'original style',
    settings: { githubTokenEnv: 'GITHUB_TOKEN', providers: [], roles: { chat: { provider: '', model: '' }, review: { provider: '', model: '' }, jev: { provider: '', model: '' } }, acp: { command: 'agent', args: [], managedRoot: '/isolated' } },
    workspace: { status: 'ready', root: '/isolated', path: '/isolated/pr', id: 'workspace-1' }, analysis: analysis(),
    ...patch,
  };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((accept, decline) => { resolve = accept; reject = decline; });
  return { promise, resolve, reject };
}

function setup(t: TestContext, initial = state(), instance = new Store()) {
  instance.state = initial;
  instance.booting = false;
  instance.verificationJobs = [];
  instance.permissions = [];
  instance.activityOpen = false;
  instance.lastAcceptedJobId = null;
  const handlers: Record<string, (body: unknown, path: string) => unknown> = {};
  const requests: { method: string; path: string; body: unknown; signal?: AbortSignal }[] = [];
  const timers = new Map<number, { fn: () => void; delay: number }>();
  let timerId = 0;
  t.mock.method(window, 'setTimeout', (fn: () => void, delay: number) => { timers.set(++timerId, { fn, delay }); return timerId; });
  t.mock.method(globalThis, 'clearTimeout', (id: number) => { timers.delete(id); });
  t.mock.method(globalThis, 'setInterval', () => ++timerId);
  t.mock.method(globalThis, 'clearInterval', () => {});
  const transport: Transport = {
    async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
      requests.push({ method, path, body, signal });
      const route = `${method} ${path.split('?')[0]}`;
      if (handlers[route]) return await handlers[route]!(body, path) as T;
      const params = new URLSearchParams(path.split('?')[1]);
      const requestIdentity = { prKey: params.get('prKey'), baseSha: params.get('baseSha'), headSha: params.get('headSha') };
      if (route === 'GET /review/progress') return { ...requestIdentity, run: null } as T;
      if (route === 'GET /review-comments') return { ...requestIdentity, threads: [], warnings: [] } as T;
      if (route === 'GET /verifications') return { ...requestIdentity, threads: [], comments: [], jobs: [] } as T;
      if (route === 'GET /permissions') return { permissions: [permission] } as T;
      if (route === 'GET /workspace') return { ...identity, workspace: { ...initial.workspace, status: 'in_use' } } as T;
      throw new Error(`Unexpected request: ${route}`);
    },
  };
  setTransport(transport);
  t.after(() => instance.dispose());
  return { store: instance, handlers, requests, timers };
}

// Promise turns, not wall-clock sleeps: all network completions and scheduled polls are controlled by each test.
async function settle() {
  for (let turn = 0; turn < 12; turn++) await Promise.resolve();
}

test('action conflicts distinguish context/publication/checkout leases from independent AI work', () => {
  const context = { busy: {}, reviewRunning: false, reviewUnavailable: false };
  for (const running of ['review', 'verify', 'chat'] as Busy[]) {
    for (const action of ['review', 'verify', 'chat', 'createComment', 'comment', 'reply', 'refresh'] as Busy[]) {
      const blocked = conflictReason(action, { ...context, busy: { [running]: true } });
      assert.equal(!!blocked, running === action, `${running} / ${action}`);
    }
    for (const action of ['load', 'settings', 'style'] as Busy[]) assert.ok(conflictReason(action, { ...context, busy: { [running]: true } }));
    for (const action of ['publish', 'finish'] as Busy[]) assert.equal(!!conflictReason(action, { ...context, busy: { [running]: true } }), running !== 'chat');
  }
  assert.match(conflictReason('publish', { ...context, verificationRunning: 2, verificationPermissionPending: true })!, /permission/);
  for (const action of ['verify', 'load', 'settings', 'style', 'review', 'chat', 'comment', 'reply'] as Busy[]) assert.equal(conflictReason(action, { ...context, verificationRunning: 2, verificationPermissionPending: true }), null);
  assert.match(conflictReason('review', { ...context, reviewRunning: true, reviewUnavailable: true })!, /progress is unavailable/);
  assert.equal(conflictReason('chat', { ...context, reviewRunning: true, reviewUnavailable: true }), null);
  for (const action of ['publish', 'finish'] as Busy[]) assert.match(conflictReason(action, { ...context, workspaceStatus: 'in_use' })!, /checkout is in use/);
  for (const action of ['verify', 'load', 'settings', 'style', 'review', 'chat', 'comment', 'reply'] as Busy[]) assert.equal(conflictReason(action, { ...context, workspaceStatus: 'in_use' }), null);
  for (const action of ['publish', 'load', 'settings', 'style'] as Busy[]) assert.match(conflictReason(action, { ...context, busy: { reply: true } })!, /reply is being posted/);
  assert.equal(conflictReason('finish', { ...context, busy: { reply: true } }), null);
});

test('full-state and entity clocks merge independently in both response orders', () => {
  const older = state(2, { comments: [comment('editable'), suggestion('review')], chats: [thread('existing')] });
  const newer = state(5, { comments: [comment('editable', 3, 'saved edit'), comment('manual')], chats: [thread('existing', 3), thread('verification', 1, 'verification')], style: 'new style' });
  for (const [first, second] of [[older, newer], [newer, older]]) {
    const merged = mergeState(first!, second!);
    assert.equal(merged.revision, 5);
    assert.equal(merged.style, 'new style');
    assert.equal(merged.comments.filter(isLiveComment).find((c) => c.id === 'editable')?.body, 'saved edit');
    assert.deepEqual(merged.comments.map((c) => c.id).sort(), ['editable', 'manual', 'review']);
    assert.equal(merged.chats.find((c) => c.id === 'existing')?.revision, 3);
    assert.ok(merged.chats.some((c) => c.id === 'verification'));
  }
  const partial = { ...older, comments: mergeEntities(older.comments, [comment('editable', 4, 'latest partial')]) };
  assert.equal(partial.revision, 2);
  assert.equal(mergeState(partial, newer).comments.filter(isLiveComment).find((c) => c.id === 'editable')?.body, 'latest partial');
  assert.equal(mergeEntities([comment('editable', 4, 'new')], [comment('editable', 2, 'old')])[0]?.body, 'new');
  const scoped = state(1, { analysis: analysis('generation-1', 8) });
  assert.equal(mergeState(scoped, state(2)).analysis?.revision, 8, 'durable commits must retain newer scoped analysis');
  const equal = mergeState(partial, { ...older, comments: [comment('same-state-new-id')] });
  assert.ok(equal.comments.some((c) => c.id === 'same-state-new-id'));
  assert.equal(equal.comments.find((c) => c.id === 'editable')?.revision, 4);
  const other = state(6, { pullRequest: { ...newer.pullRequest!, number: 2 }, comments: [], chats: [] });
  assert.deepEqual(mergeState(newer, other).comments, []);
});

for (const order of ['review-first', 'verification-first'] as const) {
  test(`held review permits chat, verification, manual edits and replies (${order})`, async (t) => {
    const { store, handlers, requests } = setup(t);
    const review = deferred<AppState>();
    const verification = deferred<VerificationAccepted>();
    const chat = deferred<ChatThread>();
    handlers['POST /review'] = () => review.promise;
    handlers['POST /verify'] = () => verification.promise;
    handlers['POST /chat'] = () => chat.promise;
    handlers['PATCH /comments/editable'] = () => comment('editable', 2, 'my saved edit');
    handlers['POST /comments'] = () => comment('manual');
    handlers['POST /review-comments/replies'] = () => ({ id: 'reply-1', body: 'posted reply', author: 'me', createdAt: '', updatedAt: '', url: '' });
    store.publishedThreads = { ...identity, warnings: [], threads: [{ id: 'discussion', path: anchor.path, side: 'RIGHT', outdated: false, canReply: true, comments: [] }] };
    const reviewing = store.review();
    const verifying = store.verify('check this', anchor);
    store.tab = 'chat';
    store.activeChatId = 'existing';
    store.chatInput = 'ask this';
    const chatting = store.chat('ask this');
    assert.equal(await store.chat('duplicate'), false);
    assert.equal(await store.verify('duplicate', anchor), false);
    await store.review();
    assert.equal(requests.filter((r) => r.path === '/review').length, 1);
    assert.equal(requests.filter((r) => r.path === '/verify').length, 1);
    assert.equal(requests.filter((r) => r.path === '/chat').length, 1);
    assert.equal(await store.patchComment('editable', { body: 'my saved edit' }), true);
    store.openComposer(anchor, 'line');
    store.composer!.body = 'manual';
    assert.equal(await store.createComment(), true);
    store.beginReply('discussion');
    store.replyDrafts[store.replyKey('discussion')]!.text = 'posted reply';
    assert.equal(await store.postReply('discussion'), true);
    assert.equal(store.publishedThreads.threads[0]?.comments[0]?.id, 'reply-1');
    assert.equal(await store.publish('COMMENT', 'not yet'), false);
    assert.equal(await store.removeCheckout(), false);
    assert.match(store.errors.publish!, /review|verification/);
    assert.match(store.errors.finish!, /review|verification/);
    assert.equal(store.state!.revision, 1, 'partial writes must not advance the State clock');
    store.activeChatId = null;
    store.chatInput = 'unsent new conversation';
    const reviewed = state(2, { comments: [comment('editable'), suggestion('review')] });
    const verified = accepted('verified', 'complete', 3);
    handlers['GET /verifications'] = () => ({ ...identity, jobs: [verified.job], threads: [verified.thread], comments: [suggestion('verified', 'verification')] });
    if (order === 'review-first') {
      review.resolve(reviewed); await reviewing;
      assert.equal(store.busy.verify, true);
      assert.equal(store.permissions[0]?.id, permission.id);
      verification.resolve(accepted('verified')); await verifying;
      await store.pollVerifications();
    } else {
      verification.resolve(accepted('verified')); await verifying;
      await store.pollVerifications();
      review.resolve(reviewed); await reviewing;
    }
    chat.resolve(thread('existing', 2));
    assert.equal(await chatting, true);
    assert.equal(store.tab, 'chat');
    assert.equal(store.activeChatId, null);
    assert.equal(store.chatInput, 'unsent new conversation');
    assert.equal(store.comments.find((c) => c.id === 'editable')?.body, 'my saved edit');
    assert.deepEqual(store.comments.map((c) => c.id).sort(), ['editable', 'manual', 'review', 'verified']);
    assert.deepEqual(store.pendingComments.map((c) => c.id).sort(), ['editable', 'manual']);
    assert.equal(store.comments.find((c) => c.id === 'review')?.status, 'suggested');
    assert.equal(store.comments.find((c) => c.id === 'verified')?.status, 'suggested');
    assert.equal(store.globalThreads.find((c) => c.id === 'existing')?.revision, 2);
    assert.ok(store.verifyThreads.some((c) => c.id === 'verified'));
    assert.equal(store.state!.revision, 2, 'verification updates never advance the full State clock');
    assert.equal(store.activeVerifyId, null, 'acceptance and completion do not select a thread');
  });
}

test('review completion cannot rewind a live workspace poll or end verifier permissions', async (t) => {
  const { store, handlers, timers, requests } = setup(t);
  const review = deferred<AppState>();
  const verification = accepted('verified');
  const workspace = deferred<unknown>();
  handlers['POST /review'] = () => review.promise;
  handlers['POST /verify'] = () => verification;
  handlers['GET /workspace'] = () => workspace.promise;
  const reviewing = store.review();
  await store.verify('check', anchor);
  await settle();
  const timer = [...timers.entries()].find(([, value]) => value.delay === 700)!;
  timers.delete(timer[0]); timer[1].fn();
  workspace.resolve({ ...identity, workspace: { ...store.workspace, status: 'in_use' } });
  await settle();
  assert.equal(store.workspace?.status, 'in_use');
  const nextTimer = [...timers.entries()].find(([, value]) => value.delay === 700)!;
  const laterPoll = deferred<unknown>();
  handlers['GET /workspace'] = () => laterPoll.promise;
  timers.delete(nextTimer[0]); nextTimer[1].fn();
  const poll = requests.filter((r) => r.path === '/workspace').at(-1)!;
  review.resolve(state(2));
  await reviewing;
  assert.equal(store.workspace?.status, 'in_use');
  assert.equal(poll.signal?.aborted, false, 'unrelated State adoption must leave scoped polling alive');
  assert.equal(store.permissions[0]?.id, permission.id);
  const complete = accepted('verified', 'complete', 3);
  handlers['GET /verifications'] = () => ({ ...identity, jobs: [complete.job], threads: [complete.thread], comments: [] });
  await store.pollVerifications();
  laterPoll.resolve({ ...identity, workspace: { ...store.workspace, status: 'in_use' } });
  await settle();
  assert.equal(store.workspace?.status, 'in_use', 'job metadata must not guess aggregate checkout state');
  assert.equal(store.permissions[0]?.id, permission.id, 'job and review completion cannot clear unrelated permission requests');
  handlers['GET /workspace'] = () => ({ ...identity, workspace: { ...store.workspace, status: 'ready' } });
  const finalTimer = [...timers.entries()].find(([, value]) => value.delay === 700)!;
  timers.delete(finalTimer[0]); finalTimer[1].fn();
  await settle();
  assert.equal(store.workspace?.status, 'ready');
  handlers['GET /permissions'] = () => ({ permissions: [] });
  await store.pollPermissions();
  assert.deepEqual([...store.permissions], []);
});

test('accepted checks dismiss only submitted drafts and allow independent jobs without navigation', async (t) => {
  const { store, handlers, requests } = setup(t);
  const admission = deferred<VerificationAccepted>();
  handlers['POST /verify'] = () => admission.promise;
  store.selectQuestion(anchor, 'selected-line');
  store.questionDraft!.question = 'first';
  store.tab = 'chat';
  store.activeChatId = 'existing';
  store.selectedPath = anchor.path;
  const sending = store.askQuestion();
  assert.equal(store.questionDraft?.question, 'first');
  admission.resolve(accepted('first'));
  assert.equal(await sending, true);
  assert.equal(store.questionDraft, null);
  assert.equal(store.busy.verify, false);
  assert.equal(store.activeVerifyId, null);
  assert.equal(store.tab, 'chat');
  assert.equal(store.activeChatId, 'existing');
  assert.equal(store.selectedPath, anchor.path);
  assert.equal(await store.verify('duplicate turn', anchor, 'first'), false);
  assert.match(store.verificationThreadErrors.first!, /active check/);
  for (const id of ['second', 'third', 'fourth']) {
    handlers['POST /verify'] = () => accepted(id);
    assert.equal(await store.verify(id, anchor), true);
  }
  assert.equal(store.activeVerificationJobs.length, 4);
  assert.equal(requests.filter((request) => request.path === '/verify').length, 4);
  assert.deepEqual(requests.find((request) => request.path === '/verify')?.body, {
    ...identity, path: anchor.path, startLine: 1, endLine: 2, side: 'RIGHT', question: 'first', threadId: undefined,
  });
  const next = deferred<VerificationAccepted>();
  handlers['POST /verify'] = () => next.promise;
  store.selectQuestion(anchor, 'selected-line');
  store.questionDraft!.question = 'fifth';
  const changed = store.askQuestion();
  store.questionDraft!.question = 'new unsent question';
  next.resolve(accepted('fifth'));
  await changed;
  assert.equal(store.questionDraft?.question, 'new unsent question');
});

test('scoped verification polls reject reverse old snapshots and preserve edits and tombstones', async (t) => {
  const { store, handlers, requests } = setup(t);
  const old = deferred<VerificationUpdate>();
  handlers['GET /verifications'] = () => old.promise;
  const polling = store.pollVerifications();
  const oldRequest = requests.at(-1)!;
  const secondIdentity = { ...identity, prKey: 'github/team/repo/2' };
  const second = accepted('second', 'complete', 4);
  second.job = { ...second.job, ...secondIdentity };
  handlers['GET /verifications'] = () => ({
    ...secondIdentity, jobs: [second.job], threads: [second.thread],
    comments: [comment('edited', 1, 'old body'), suggestion('denied', 'verification')],
  });
  handlers['GET /state'] = () => state(5, {
    pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' },
    chats: [], comments: [comment('edited', 3, 'saved body'), { id: 'denied', revision: 3, status: 'deleted' }],
  });
  await store.boot();
  await settle();
  assert.equal(oldRequest.signal?.aborted, true);
  const first = accepted('first', 'complete', 5);
  old.resolve({ ...identity, jobs: [first.job], threads: [first.thread], comments: [suggestion('wrong-pr')] });
  await polling;
  assert.deepEqual(store.verifyThreads.map((entry) => entry.id), ['second']);
  assert.deepEqual(store.comments.map((entry) => entry.body), ['saved body']);
  assert.equal(store.state!.revision, 5);
  assert.equal(store.state!.comments.find((entry) => entry.id === 'denied')?.status, 'deleted');
  assert.equal(store.activeVerifyId, null);
  const complete = accepted('stable', 'complete', 5).job;
  const running = accepted('stable', 'running', 2).job;
  assert.equal(mergeVerificationJobs([complete], [running])[0]?.status, 'complete');
  assert.equal(mergeVerificationJobs([running], [complete])[0]?.status, 'complete');
});

test('global jobs reload on Home and block finishing only their own PR', async (t) => {
  const { store, handlers } = setup(t);
  const first = accepted('first', 'running', 2);
  handlers['GET /state'] = () => state();
  handlers['GET /verifications'] = () => ({ ...identity, jobs: [first.job], threads: [first.thread], comments: [] });
  await store.boot();
  await settle();
  store.goHome();
  assert.equal(store.activeVerificationJobs.length, 1);
  assert.match(store.publicationBlockReason!, /active|permission/);
  handlers['GET /verifications'] = () => ({ ...identity, prKey: 'github/team/repo/2', jobs: [first.job], threads: [], comments: [] });
  handlers['POST /pull-request'] = () => state(3, {
    pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' }, chats: [], comments: [],
  });
  assert.equal(await store.openPullRequest('https://github.com/team/repo/pull/2', 'guide'), true);
  assert.equal(store.prTab, 'guide');
  await settle();
  assert.equal(store.activeVerificationJobs.length, 1);
  assert.equal(store.prVerificationJobs.length, 0);
  assert.equal(store.publicationBlockReason, null);
  assert.equal(store.blockReason('finish'), null);
  assert.deepEqual([...store.verifyThreads], []);
});

test('permission decisions and cancellation belong to job IDs even when sessions match', async (t) => {
  const { store, handlers, requests } = setup(t);
  const first = accepted('verified', 'running', 2).job;
  const second = accepted('second', 'running', 2).job;
  store.verificationJobs = [first, second];
  const other = { ...permission, id: 'permission-2', jobId: second.id, threadId: second.threadId };
  store.permissions = [permission, other];
  const stalePermissions = deferred<{ permissions: Permission[] }>();
  handlers['GET /permissions'] = () => stalePermissions.promise;
  const polling = store.pollPermissions();
  handlers['POST /permissions/permission-1'] = () => ({});
  await store.answerPermission(permission.id, 'yes');
  stalePermissions.resolve({ permissions: [permission, other] });
  await polling;
  assert.deepEqual(store.permissions.map((entry) => entry.id), [other.id]);
  const cancelling = { ...first, status: 'cancelling' as const, updatedAt: '2026-01-01T00:00:03Z' };
  handlers[`POST /verifications/${first.id}/cancel`] = () => cancelling;
  await store.cancelVerification(first.id);
  assert.equal(store.verificationJobs.find((job) => job.id === first.id)?.status, 'cancelling');
  assert.equal(store.verificationJobs.find((job) => job.id === second.id)?.status, 'running');
  assert.deepEqual(requests.find((request) => request.path.endsWith('/cancel'))?.body, identity);
  handlers[`POST /verifications/${second.id}/cancel`] = () => { throw new Error('lease could not drain'); };
  await store.cancelVerification(second.id);
  assert.match(store.verificationErrors[second.id]!, /lease could not drain/);
  assert.equal(store.verificationJobs.find((job) => job.id === second.id)?.status, 'running');
});

test('nonmodal activity exposes scoped controls while acceptance and completion leave review focus alone', async (t) => {
  const { store, handlers } = setup(t, state(), mountedStore);
  store.busy = {};
  store.errors = {};
  store.questionDraft = null;
  store.composer = null;
  store.activeVerifyId = null;
  store.tab = 'chat';
  store.prTab = 'guide';
  handlers['GET /state'] = () => state();
  handlers['GET /permissions'] = () => ({ permissions: [] });
  await store.boot();
  await settle();
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: QuestionPanel } = await import('../src/components/QuestionPanel.svelte');
  const { default: VerificationActivity } = await import('../src/components/VerificationActivity.svelte');
  const { default: VerifyPanel } = await import('../src/components/VerifyPanel.svelte');
  const target = document.createElement('div');
  const reading = document.createElement('button');
  reading.textContent = 'Continue reviewing';
  target.append(reading);
  document.body.append(target);
  const panels = [mount(QuestionPanel, { target }), mount(VerificationActivity, { target }), mount(VerifyPanel, { target })];
  t.after(async () => {
    for (const panel of panels) await unmount(panel);
    target.remove();
    store.questionDraft = null;
    store.activeVerifyId = null;
  });
  store.selectQuestion(anchor, 'reading');
  store.questionDraft!.question = 'verified';
  flushSync();
  await settle();
  const admission = deferred<VerificationAccepted>();
  handlers['POST /verify'] = () => admission.promise;
  target.querySelector('form.question-panel')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
  reading.focus();
  admission.resolve(accepted('verified'));
  await settle(); flushSync();
  assert.equal(target.querySelector('.question-panel'), null);
  assert.equal(document.activeElement, reading);
  assert.equal(store.tab, 'chat');
  assert.equal(store.activeVerifyId, null);
  store.activityOpen = true;
  const second = accepted('second', 'running', 2);
  store.verificationJobs = [...store.verificationJobs, second.job];
  store.permissions = [permission, { ...permission, id: 'other-permission', jobId: second.job.id, threadId: second.thread.id }];
  flushSync();
  assert.equal(target.querySelector('[role="dialog"], [aria-modal="true"]'), null);
  assert.equal(document.activeElement, reading);
  const activity = target.querySelector<HTMLElement>('#verification-activity')!;
  assert.equal(activity.hidden, false);
  assert.equal(activity.querySelectorAll('[data-job-id]').length, 2);
  assert.equal(activity.querySelectorAll('[data-permission-id]').length, 2);
  const firstCard = activity.querySelector('[data-job-id="job-verified"]')!;
  assert.equal(firstCard.querySelectorAll('[data-permission-id]').length, 1);
  assert.equal(firstCard.querySelector('[data-permission-id]')?.getAttribute('data-permission-id'), permission.id);
  const complete = accepted('verified', 'complete', 4);
  complete.thread.messages.push({ role: 'assistant', content: 'Detailed command report: tests passed.' });
  handlers['GET /verifications'] = () => ({ ...identity, jobs: [complete.job, second.job], threads: [complete.thread, second.thread], comments: [] });
  await store.pollVerifications(); flushSync();
  assert.equal(document.activeElement, reading);
  assert.equal(store.tab, 'chat');
  assert.equal(store.activeVerifyId, null);
  assert.equal(store.prTab, 'guide');
  assert.equal(store.comments.some((entry) => entry.body.includes('Detailed command report')), false);
  [...firstCard.querySelectorAll('button')].find((button) => button.textContent === 'Open thread')!.click();
  await settle(); flushSync();
  assert.equal(store.tab, 'verify');
  assert.equal(store.prTab, 'diff');
  assert.equal(store.mobileView, 'panel');
  assert.equal(store.activeVerifyId, 'verified');
  assert.equal(activity.hidden, true);
  assert.match(target.querySelector('.conversation-list')!.textContent!, /Detailed command report/);
  store.verifyInput = 'another check';
  flushSync();
  const followup = target.querySelector<HTMLTextAreaElement>('#vc')!;
  assert.equal(followup.form!.querySelector<HTMLButtonElement>('button[type="submit"]')!.disabled, false);
  const continued = accepted('verified', 'queued', 5);
  continued.job = { ...continued.job, id: 'follow-up-job', startedAt: '2026-01-01T00:01:00Z', updatedAt: '2026-01-01T00:01:00Z' };
  continued.thread.verification = continued.job;
  handlers['POST /verify'] = () => continued;
  followup.form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle(); flushSync();
  assert.equal(followup.value, '');
  assert.equal(followup.form!.querySelector<HTMLButtonElement>('button[type="submit"]')!.disabled, true);
  assert.equal(store.activeVerificationJobs.length, 2);
  assert.equal(store.verificationBlockReason, null, 'a different new thread remains available');
});

for (const order of ['refresh-first', 'review-first'] as const) {
  test(`analysis refresh stays independent and scoped (${order})`, async (t) => {
    const { store, handlers } = setup(t);
    const review = deferred<AppState>();
    const refresh = deferred<unknown>();
    handlers['POST /review'] = () => review.promise;
    handlers['POST /analysis'] = () => refresh.promise;
    const reviewing = store.review();
    const refreshing = store.refreshAnalysis();
    const updated = { analysis: analysis('generation-2', 4), files: [{ path: anchor.path, risk: { level: 'high', probability: 0.9, factors: [{ text: 'new result', side: 'RIGHT', startLine: 2, endLine: 2 }] } }], groups: [{ id: 'new', title: 'New order', paths: [anchor.path] }] };
    if (order === 'refresh-first') { refresh.resolve(updated); await refreshing; review.resolve(state(2)); await reviewing; }
    else { review.resolve(state(2)); await reviewing; refresh.resolve(updated); await refreshing; }
    assert.equal(store.state!.analysis?.generation, 'generation-2');
    assert.equal(store.files[0]?.risk?.factors?.[0]?.text, 'new result');
    assert.equal(store.state!.groups[0]?.id, 'new');
  });
}

test('activity groups checks by status and clearing hides only finished ones', async (t) => {
  const { store, handlers } = setup(t, state(), mountedStore);
  handlers['GET /state'] = () => state();
  handlers['GET /permissions'] = () => ({ permissions: [] });
  await store.boot();
  await settle();
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: VerificationActivity } = await import('../src/components/VerificationActivity.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panel = mount(VerificationActivity, { target });
  t.after(async () => { await unmount(panel); target.remove(); store.activityOpen = false; });
  const jobs = { complete: accepted('done', 'complete', 3).job, failed: accepted('broke', 'failed', 3).job, running: accepted('busy', 'running', 2).job };
  const earlier = { ...accepted('old', 'complete', 3).job, cleared: true };
  store.permissions = [];
  store.verificationJobs = [jobs.complete, jobs.failed, jobs.running, earlier];
  store.activityOpen = true;
  flushSync();
  const headings = () => [...target.querySelectorAll('#verification-activity h3')].map((h) => h.textContent!.replace(/\s*\d+$/, ''));
  assert.deepEqual(headings(), ['In progress', 'Failed', 'Complete']);
  assert.equal(target.querySelector(`[data-job-id="${earlier.id}"]`), null, 'previously cleared checks stay hidden');
  handlers['POST /verifications/clear'] = () => ({ jobs: [{ ...jobs.complete, cleared: true, revision: 4 }, { ...jobs.failed, cleared: true, revision: 4 }] });
  [...target.querySelectorAll('button')].find((button) => button.textContent?.includes('Clear finished'))!.click();
  await settle(); flushSync();
  assert.deepEqual(headings(), ['In progress']);
  assert.ok(target.querySelector(`[data-job-id="${jobs.running.id}"]`), 'running checks are never cleared');
  assert.equal([...target.querySelectorAll('button')].some((button) => button.textContent?.includes('Clear finished')), false);
});

test('late partial responses cannot enter another PR, including an A-to-B-to-A switch', async (t) => {
  const { store, handlers } = setup(t);
  const chat = deferred<ChatThread>();
  const patch = deferred<ReviewComment>();
  handlers['POST /chat'] = () => chat.promise;
  handlers['PATCH /comments/editable'] = () => patch.promise;
  const chatting = store.chat('old request');
  const editing = store.patchComment('editable', { body: 'old request' });
  // Another window can change server context; boot reacquires the authoritative snapshot.
  handlers['GET /state'] = () => state(8, { pullRequest: { ...state().pullRequest!, number: 2 }, comments: [], chats: [] });
  await store.boot();
  handlers['GET /state'] = () => state(9, { comments: [comment('editable', 5, 'new context edit')], chats: [] });
  await store.boot();
  chat.resolve(thread('late', 99)); patch.resolve(comment('editable', 99, 'late old edit'));
  assert.equal(await chatting, false);
  assert.equal(await editing, false);
  assert.equal(store.comments[0]?.body, 'new context edit');
  assert.deepEqual([...store.state!.chats], []);
});

test('late background full snapshots cannot roll back a PR switch or partial settings save', async (t) => {
  const { store, handlers } = setup(t);
  const snapshot = deferred<AppState>();
  handlers['GET /review/progress'] = () => ({ ...identity, run: finished });
  handlers['GET /state'] = () => snapshot.promise;
  store.reviewProgress = { ...finished, status: 'running' };
  const progress = store.refreshReviewProgress();
  await settle();
  handlers['PUT /settings'] = () => store.state!.settings;
  const saving = store.saveSettings(store.state!.settings, { style: 'new saved style' });
  await settle();
  snapshot.resolve(state(2));
  await progress;
  assert.equal(await saving, true);
  assert.equal(store.state!.style, 'new saved style');
  const staleSnapshot = deferred<AppState>();
  handlers['GET /state'] = () => staleSnapshot.promise;
  store.reviewProgress = { ...finished, status: 'running' };
  const staleProgress = store.refreshReviewProgress();
  await settle();
  handlers['POST /pull-request'] = () => state(5, { pullRequest: { ...state().pullRequest!, number: 2 }, comments: [], chats: [] });
  assert.equal(await store.loadPullRequest('https://github.com/team/repo/pull/2'), true);
  staleSnapshot.resolve(state(3, { comments: [suggestion('late-review')] }));
  await staleProgress;
  assert.equal(store.pr?.number, 2);
  assert.deepEqual([...store.comments], []);
});

test('server conflicts preserve reply text, and uncertain delivery still requires a checked refresh', async (t) => {
  const { store, handlers } = setup(t);
  store.publishedThreads = { ...identity, warnings: [], threads: [{ id: 'discussion', path: anchor.path, side: 'RIGHT', outdated: false, canReply: true, comments: [] }] };
  store.beginReply('discussion');
  const draft = store.replyDrafts[store.replyKey('discussion')]!;
  draft.text = 'keep this reply';
  handlers['POST /review-comments/replies'] = () => { throw new ApiError('Publish is running in another window.', 409); };
  assert.equal(await store.postReply('discussion'), false);
  assert.match(draft.error!, /Publish is running/);
  assert.equal(draft.text, 'keep this reply');
  assert.equal(draft.uncertain, false);
  handlers['POST /review-comments/replies'] = () => { throw new ApiError('Unknown delivery.', 500, 'reply_delivery_uncertain'); };
  assert.equal(await store.postReply('discussion'), false);
  assert.equal(draft.uncertain, true);
  assert.equal(await store.postReply('discussion'), false);
  await store.loadPublishedThreads(true);
  assert.equal(draft.refreshed, true);
  assert.equal(draft.uncertain, true, 'refresh does not silently authorize a duplicate post');
});

test('mounted review panels retain unsent inputs and manual edits across unrelated completions', async (t) => {
  const { store, handlers } = setup(t, state(), mountedStore);
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: RightPanel } = await import('../src/components/RightPanel.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panel = mount(RightPanel, { target });
  t.after(async () => { await unmount(panel); target.remove(); });
  flushSync();
  store.chatInput = 'unsent chat';
  store.tab = 'review';
  flushSync();
  const edit = [...target.querySelectorAll('button')].find((button) => button.getAttribute('aria-label') === 'Edit comment')!;
  edit.click(); flushSync();
  const textarea = target.querySelector<HTMLTextAreaElement>('[data-comment-id="editable"] textarea')!;
  textarea.value = 'unsaved manual edit'; textarea.dispatchEvent(new Event('input', { bubbles: true }));
  const review = deferred<AppState>();
  handlers['POST /review'] = () => review.promise;
  const reviewing = store.review();
  flushSync();
  assert.equal(textarea.disabled, false);
  assert.equal(target.querySelector<HTMLTextAreaElement>('#chat-in')!.disabled, false);
  assert.equal(target.querySelector<HTMLButtonElement>('.chat-composer button')!.disabled, false);
  store.tab = 'chat'; flushSync();
  review.resolve(state(2, { comments: [comment('editable'), suggestion('review')] }));
  await reviewing; flushSync();
  assert.equal(store.tab, 'chat');
  assert.equal(target.querySelector<HTMLTextAreaElement>('#chat-in')!.value, 'unsent chat');
  store.tab = 'review'; flushSync();
  assert.equal(target.querySelector<HTMLTextAreaElement>('[data-comment-id="editable"] textarea')!.value, 'unsaved manual edit');
});

test('late comment and chat responses cannot replace higher entity revisions already in a full snapshot', async (t) => {
  const { store, handlers } = setup(t);
  const chat = deferred<ChatThread>();
  const patch = deferred<ReviewComment>();
  handlers['POST /chat'] = () => chat.promise;
  handlers['PATCH /comments/editable'] = () => patch.promise;
  handlers['POST /review'] = () => state(6, {
    comments: [comment('editable', 5, 'newest saved edit')],
    chats: [thread('existing', 5), thread('verified', 1, 'verification')],
  });
  store.activeChatId = 'existing';
  const chatting = store.chat('a prior turn');
  const editing = store.patchComment('editable', { body: 'an earlier saved edit' });
  await store.review();
  chat.resolve(thread('existing', 2));
  patch.resolve(comment('editable', 2, 'an earlier saved edit'));
  await Promise.all([chatting, editing]);
  assert.equal(store.globalThreads[0]?.revision, 5);
  assert.equal(store.comments[0]?.body, 'newest saved edit');
  assert.equal(store.state!.revision, 6);
});

test('a discussion refresh captured before a posted reply cannot erase that reply', async (t) => {
  const { store, handlers } = setup(t);
  const discussion = { id: 'discussion', path: anchor.path, side: 'RIGHT' as const, outdated: false, canReply: true, comments: [] };
  store.publishedThreads = { ...identity, warnings: [], threads: [discussion] };
  store.beginReply('discussion');
  store.replyDrafts[store.replyKey('discussion')]!.text = 'posted text';
  const stale = deferred<unknown>();
  handlers['GET /review-comments'] = () => stale.promise;
  const refreshing = store.loadPublishedThreads(true);
  const posted = { id: 'posted', body: 'posted text', author: 'me', createdAt: '', updatedAt: '', url: '' };
  handlers['POST /review-comments/replies'] = () => posted;
  handlers['GET /review-comments'] = () => ({ ...identity, warnings: [], threads: [{ ...discussion, comments: [posted] }] });
  assert.equal(await store.postReply('discussion'), true);
  await settle();
  stale.resolve({ ...identity, warnings: [], threads: [discussion] });
  await refreshing;
  assert.equal(store.publishedThreads.threads[0]?.comments[0]?.id, 'posted');
});

test('deletion tombstones stay hidden and defeat delayed review snapshots and older partial edits', async (t) => {
  const { store, handlers, requests } = setup(t);
  const review = deferred<AppState>();
  const tombstone: DeletedComment = { id: 'editable', revision: 3, status: 'deleted' };
  handlers['POST /review'] = () => review.promise;
  handlers['DELETE /comments/editable'] = () => tombstone;
  const reviewing = store.review();
  assert.equal(await store.deleteComment('editable'), true);
  assert.deepEqual([...store.comments], []);
  assert.deepEqual(store.state!.comments[0], tombstone);
  assert.equal(store.state!.revision, 1, 'DELETE is an entity response, not a full-state clock');
  assert.deepEqual(requests.find((request) => request.method === 'DELETE')?.body, {});
  review.resolve(state(2, { comments: [comment('editable'), suggestion('new-suggestion')] }));
  await reviewing;
  assert.deepEqual(store.comments.map((entry) => entry.id), ['new-suggestion']);
  const merged = mergeEntities(store.state!.comments, [comment('editable', 2, 'delayed pre-delete edit')]);
  assert.deepEqual(merged.find((entry) => entry.id === 'editable'), tombstone);
  assert.equal(await store.deleteComment('editable'), true, 'repeat delete retains the same tombstone');
  assert.deepEqual(Object.keys(store.state!.comments.find((entry) => entry.id === 'editable')!).sort(), ['id', 'revision', 'status']);
});

test('Finish Review retains stale pending blockers, validates outcomes and permits a bare approval', async (t) => {
  const stale = { ...comment('stale-pending'), stale: true };
  const { store, handlers, requests } = setup(t, state(1, { comments: [stale] }));
  store.busy.review = true;
  store.openFinishReview();
  assert.equal(store.finishReviewContext?.number, 1, 'the overview is inspectable during a review');
  assert.equal(store.pendingComments[0]?.id, 'stale-pending');
  assert.match(store.publicationBlockReason!, /review is running/);
  store.busy.review = false;
  assert.match(store.publicationBlockReason!, /Pending comments belong to earlier code/);
  assert.equal(await store.publish('APPROVE', ''), false);
  handlers['DELETE /comments/stale-pending'] = () => ({ id: 'stale-pending', revision: 2, status: 'deleted' });
  assert.equal(await store.deleteComment('stale-pending'), true);
  assert.equal(await store.publish('REQUEST_CHANGES', ''), false);
  assert.match(store.errors.publish!, /summary/);
  assert.equal(await store.publish('COMMENT', ''), false);
  assert.match(store.errors.publish!, /pending comment or summary/);
  handlers['POST /publish'] = () => state(4, { comments: store.state!.comments });
  assert.equal(await store.publish('APPROVE', ''), true);
  assert.deepEqual(requests.filter((request) => request.path === '/publish').map((request) => request.body), [{ ...identity, event: 'APPROVE' }]);
  const expected = store.finishReviewContext!;
  handlers['GET /state'] = () => state(5, { pullRequest: { ...state().pullRequest!, number: 2 }, comments: [] });
  await store.boot();
  assert.equal(store.finishReviewStale, true);
  assert.equal(await store.publish('APPROVE', '', expected), false);
  assert.match(store.errors.publish!, /pull request changed/);
});

test('live comment menus approve suggestions into pending, preserve status on edit and delete without resurrection', async (t) => {
  const ai = suggestion('ai');
  const published: ReviewComment = { ...comment('published'), status: 'published' };
  const initial = state(1, { comments: [comment('human'), ai, published] });
  const { store, handlers, requests } = setup(t, initial, mountedStore);
  handlers['GET /state'] = () => initial;
  await store.boot();
  // Browser component modules initialize only after the test DOM above has been installed.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: ReviewCommentsPanel } = await import('../src/components/ReviewCommentsPanel.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panel = mount(ReviewCommentsPanel, { target });
  t.after(async () => { await unmount(panel); target.remove(); });
  flushSync();
  const action = (id: string, label: string) => [...target.querySelectorAll<HTMLButtonElement>(`[data-comment-id="${id}"] button`)].find((button) => (button.getAttribute('aria-label') ?? button.textContent?.trim()) === label);
  assert.equal(action('published', 'Edit comment'), undefined);
  handlers['PATCH /comments/ai'] = () => ({ ...ai, status: 'pending', revision: 2 });
  action('ai', 'Approve')!.click();
  await settle(); flushSync();
  assert.equal(store.comments.find((entry) => entry.id === 'ai')?.status, 'pending');
  action('ai', 'Edit comment')!.click(); flushSync();
  const input = target.querySelector<HTMLTextAreaElement>('[data-comment-id="ai"] textarea')!;
  input.value = 'my edited suggestion';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  handlers['PATCH /comments/ai'] = () => ({ ...ai, status: 'pending', revision: 3, body: 'my edited suggestion' });
  input.form!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await settle(); flushSync();
  assert.equal(store.comments.find((entry) => entry.id === 'ai')?.body, 'my edited suggestion');
  assert.deepEqual(requests.filter((request) => request.path === '/comments/ai').map((request) => request.body), [{ status: 'pending' }, { body: 'my edited suggestion' }]);
  handlers['DELETE /comments/ai'] = () => ({ id: 'ai', revision: 4, status: 'deleted' });
  action('ai', 'Delete')!.click();
  await settle(); flushSync();
  assert.equal(target.querySelector('[data-comment-id="ai"]'), null);
  assert.equal(store.comments.some((entry) => entry.id === 'ai'), false);
  assert.equal(store.state!.comments.find((entry) => entry.id === 'ai')?.status, 'deleted');
});

for (const tab of ['description', 'commits', 'diff', 'guide'] as const) test(`explicit file jumps from ${tab} are fresh and ignore unknown paths`, (t) => {
  const { store } = setup(t);
  store.prTab = tab;
  store.mobileView = 'panel';
  t.mock.method(Date, 'now', () => 1234);
  const line = { side: 'RIGHT' as const, line: 2 };
  store.selectFile(anchor.path, { line });
  const first = store.fileNavigation!;
  store.selectFile(anchor.path, { line });
  const second = store.fileNavigation!;
  assert.equal(second.path, anchor.path);
  assert.deepEqual(second.line, line);
  assert.ok(second.nonce > first.nonce, 'repeated jumps cannot depend on a clock tick or a selected-path change');
  assert.notEqual(second, first);
  assert.equal(store.selectedPath, anchor.path);
  assert.equal(store.prTab, tab === 'guide' ? 'guide' : 'diff');
  assert.equal(store.mobileView, 'diff');
  store.selectFile(anchor.path);
  const header = store.fileNavigation!;
  assert.ok(header.nonce > second.nonce);
  assert.equal(header.line, undefined, 'file jumps must not retain an earlier line target');
  store.goHome();
  store.settingsOpen = true;
  store.mobileView = 'files';
  store.selectFile('missing.ts', { line });
  assert.equal(store.fileNavigation, header);
  assert.equal(store.selectedPath, anchor.path);
  assert.equal(store.view, 'home');
  assert.equal(store.settingsOpen, true, 'invalid links must not change views');
  assert.equal(store.prTab, tab === 'guide' ? 'guide' : 'diff');
  assert.equal(store.mobileView, 'files');
});

test('explicit PR tabs reveal code while resuming without a tab retains the mobile pane', (t) => {
  const { store } = setup(t);
  assert.equal(store.prTab, 'guide');
  for (const tab of ['description', 'commits', 'diff', 'guide'] as const) {
    store.mobileView = 'panel';
    store.resumePr(tab);
    assert.equal(store.prTab, tab);
    assert.equal(store.mobileView, tab === 'diff' || tab === 'guide' ? 'diff' : 'panel');
    store.mobileView = 'files';
    store.goHome();
    store.resumePr();
    assert.equal(store.view, 'pr');
    assert.equal(store.prTab, tab);
    assert.equal(store.mobileView, 'files');
  }
});

test('snapshot changes clear old jump intent without discarding anchored drafts', async (t) => {
  const { store, handlers } = setup(t);
  store.clickLine(anchor.path, 'RIGHT', 2, 0, false);
  store.questionDraft!.question = 'keep my question';
  store.openComposer(anchor, diffElementId(anchor.path, 'comment', 'RIGHT', 2));
  store.composer!.body = 'keep my comment';
  const question = store.questionDraft;
  const composer = store.composer;
  store.selectFile(anchor.path, { line: { side: 'RIGHT', line: 2 } });
  const first = store.fileNavigation!;
  handlers['GET /state'] = () => state(2);
  assert.equal(await store.boot(), true);
  assert.equal(store.fileNavigation, first, 'unrelated state adoption must not create or discard navigation');
  assert.equal(store.selectedPath, anchor.path);
  handlers['GET /state'] = () => state(3, { pullRequest: { ...state().pullRequest!, headSha: 'fresh-head' } });
  assert.equal(await store.boot(), true);
  assert.equal(store.fileNavigation, null);
  assert.equal(store.selectedPath, null, 'fresh snapshots start at the overview, not an implicit first-file jump');
  assert.equal(store.questionDraft, question);
  assert.equal(store.composer, composer);
  assert.equal(store.questionStale, true);
  assert.equal(store.composerStale, true);
  store.selectFile(anchor.path);
  assert.ok(store.fileNavigation!.nonce > first.nonce, 'the sequence survives snapshot resets');
  handlers['GET /state'] = () => state(4, { pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' } });
  assert.equal(await store.boot(), true);
  assert.equal(store.fileNavigation, null);
  assert.equal(store.selectedPath, null);
});

test('diff identities distinguish files, sides and actions without delimiter or path collisions', () => {
  const paths = ['src/a/b.ts', 'src%2Fa%2Fb.ts', 'src/a:b.ts', 'src/a-b.ts', 'src/a|b.ts', 'src/a?b#c.ts', 'src/a "quoted".ts', 'src/é.ts', 'src/e\u0301.ts', 'src/[line]-RIGHT-1.ts', 'src/a\\b.ts', 'src/a\nb.ts', 'src/\ud800.ts'];
  const ids: string[] = [];
  for (const path of paths) {
    ids.push(diffElementId(path, 'file'));
    for (const side of ['LEFT', 'RIGHT'] as const) {
      for (const line of [1, 12]) {
        for (const kind of ['line', 'select', 'comment'] as const) ids.push(diffElementId(path, kind, side, line));
      }
    }
  }
  assert.equal(new Set(ids).size, ids.length);
  assert.ok(ids.every((id) => !/\s/.test(id)), 'DOM ids cannot contain path whitespace');
});

test('mounted file diffs keep repeated line identities and return focus in the originating file', async (t) => {
  const files = [anchor.path, 'src/a "quoted"#?:|%2F.ts'].map((path) => ({ ...state().files[0]!, path }));
  const { store } = setup(t, state(1, { files, comments: [] }), mountedStore);
  store.diffLayout = 'unified';
  store.questionDraft = null;
  store.composer = null;
  store.fileNavigation = null;
  store.selectedPath = null;
  store.busy = {};
  // Mount the actual browser components after installing the test DOM.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const { default: QuestionPanel } = await import('../src/components/QuestionPanel.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panels = files.map((file) => mount(FileDiff, { target, props: { file } }));
  const questionPanel = mount(QuestionPanel, { target });
  t.after(async () => {
    await unmount(questionPanel);
    for (const panel of panels) await unmount(panel);
    target.remove();
    store.questionDraft = null;
    store.composer = null;
    store.fileNavigation = null;
    store.selectedPath = null;
  });
  flushSync();
  const allIds = [...target.querySelectorAll('[id]')].map((element) => element.id);
  assert.equal(new Set(allIds).size, allIds.length, 'simultaneous files cannot reuse line or gutter ids');
  for (const file of files) {
    const article = document.getElementById(diffElementId(file.path, 'file'))!;
    assert.equal(article.dataset.filePath, file.path);
    assert.ok(article.querySelector('table'));
    assert.equal(article.querySelector<HTMLElement>('.filebody')?.hidden, false, 'every file starts expanded');
    for (const side of ['LEFT', 'RIGHT'] as const) {
      for (const kind of ['line', 'select', 'comment'] as const) {
        const element = document.getElementById(diffElementId(file.path, kind, side, 1));
        assert.ok(element && article.contains(element), `${kind} belongs to ${file.path}, ${side}`);
      }
    }
  }
  const secondPath = files[1]!.path;
  const select = document.getElementById(diffElementId(secondPath, 'select', 'RIGHT', 1)) as HTMLButtonElement;
  select.click(); flushSync();
  await settle();
  assert.equal(store.selection?.path, secondPath);
  assert.equal(store.questionDraft?.returnFocus, select.id);
  assert.equal(store.fileNavigation, null, 'selecting code is not a file jump');
  const clear = [...target.querySelectorAll<HTMLButtonElement>('.question-panel button')].find((button) => button.getAttribute('aria-label') === 'Clear selection')!;
  clear.click(); flushSync();
  await settle();
  assert.equal(document.activeElement, select, 'clearing selection must not focus the same line in an earlier file');
  const commentButton = document.getElementById(diffElementId(secondPath, 'comment', 'RIGHT', 1)) as HTMLButtonElement;
  commentButton.click(); flushSync();
  await settle();
  assert.equal(store.composer?.anchor.path, secondPath);
  assert.equal(store.composer?.returnFocus, commentButton.id);
  assert.equal(target.querySelectorAll('.composer').length, 1);
  store.selectedPath = files[0]!.path;
  flushSync();
  assert.equal(target.querySelector('.composer')?.closest<HTMLElement>('[data-file-path]')?.dataset.filePath, secondPath);
  const cancel = [...target.querySelectorAll<HTMLButtonElement>('.composer button')].find((button) => button.getAttribute('aria-label') === 'Cancel comment')!;
  cancel.click(); flushSync();
  await settle();
  assert.equal(document.activeElement, commentButton);
  const articles = files.map((file) => document.getElementById(diffElementId(file.path, 'file'))!);
  const bodies = articles.map((article) => article.querySelector<HTMLElement>('.filebody')!);
  const toggles = articles.map((article) => article.querySelector<HTMLButtonElement>('.filehead button[aria-expanded]')!);
  for (const toggle of toggles) toggle.click();
  flushSync();
  assert.ok(bodies.every((body) => body.hidden));
  store.selectedPath = secondPath;
  flushSync();
  assert.ok(bodies.every((body) => body.hidden), 'passive selection must not expand a collapsed file');
  store.selectFile(secondPath);
  flushSync();
  assert.equal(bodies[0]!.hidden, true, 'explicit navigation only expands its target');
  assert.equal(bodies[1]!.hidden, false);
  toggles[1]!.click(); flushSync();
  assert.equal(bodies[1]!.hidden, true);
  store.selectFile(secondPath);
  flushSync();
  assert.equal(bodies[1]!.hidden, false, 'a repeated same-file jump must expand the file again');
});

test('split diffs retain canonical sides, unique anchors and mounted drafts across layout changes', async (t) => {
  const file = {
    ...state().files[0]!,
    patch: '@@ -1,4 +1,5 @@\n context\n-old two\n-old three\n+new two\n+new three\n+new four\n ending\n@@ -10,2 +11 @@\n-old ten\n-old eleven\n+new eleven',
    additions: 4, deletions: 4,
  };
  const comments = [
    { ...comment('left'), side: 'LEFT' as const, startLine: 2, endLine: 2 },
    { ...suggestion('right'), startLine: 2, endLine: 2 },
    { ...comment('context'), startLine: 1, endLine: 1 },
  ];
  const { store } = setup(t, state(1, { files: [file], comments }), mountedStore);
  store.diffLayout = 'unified';
  store.questionDraft = null;
  store.composer = null;
  store.fileNavigation = null;
  store.busy = {};
  store.replyDrafts = {};
  store.publishedThreads = {
    ...identity, warnings: [], threads: ['LEFT', 'RIGHT'].map((side) => ({
      id: `published-${side}`, path: file.path, side: side as 'LEFT' | 'RIGHT',
      startLine: 2, endLine: 2, outdated: false, canReply: true, comments: [],
    })),
  };
  // Actual Svelte components must initialize after the browser DOM installed above.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const { default: QuestionPanel } = await import('../src/components/QuestionPanel.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panel = mount(FileDiff, { target, props: { file } });
  const question = mount(QuestionPanel, { target });
  t.after(async () => {
    await unmount(question); await unmount(panel); target.remove();
    store.diffLayout = 'unified';
    store.questionDraft = null; store.composer = null;
    store.publishedThreads = null; store.replyDrafts = {};
  });
  flushSync();
  const button = (kind: 'select' | 'comment', side: 'LEFT' | 'RIGHT', line: number) =>
    document.getElementById(diffElementId(file.path, kind, side, line)) as HTMLButtonElement;
  const card = (id: string) => target.querySelector<HTMLElement>(`[data-comment-id="${id}"]`)!;
  const discussion = (side: 'LEFT' | 'RIGHT') => target.querySelector<HTMLElement>(`[data-thread-id="published-${side}"]`)!;
  const type = async (input: HTMLTextAreaElement, text: string) => {
    input.value = text; input.dispatchEvent(new Event('input', { bubbles: true })); flushSync();
    // Browser input bindings finish their tick before a later click can unmount the field.
    await settle();
  };
  const edit = [...card('left').querySelectorAll<HTMLButtonElement>('button')].find((b) => b.getAttribute('aria-label') === 'Edit comment')!;
  edit.click(); flushSync();
  const editInput = card('left').querySelector<HTMLTextAreaElement>('textarea')!;
  await type(editInput, 'unsaved old-side edit');
  discussion('LEFT').querySelector<HTMLButtonElement>('button')!.click(); flushSync();
  const replyInput = discussion('LEFT').querySelector<HTMLTextAreaElement>('textarea')!;
  await type(replyInput, 'unsaved old-side reply');
  button('select', 'LEFT', 2).click();
  button('select', 'LEFT', 3).dispatchEvent(new MouseEvent('click', { bubbles: true, shiftKey: true }));
  flushSync();
  assert.deepEqual(store.selection, { path: file.path, side: 'LEFT', start: 2, end: 3, hunk: 0 });
  button('comment', 'LEFT', 3).click(); flushSync();
  const composerInput = target.querySelector<HTMLTextAreaElement>('.composer textarea')!;
  await type(composerInput, 'unsaved range comment');
  const composer = store.composer;
  const table = target.querySelector('table')!;
  for (const layout of ['split', 'unified', 'split'] as const) {
    store.diffLayout = layout; flushSync();
    assert.equal(target.querySelector('table'), table, 'switching must reuse the single diff table');
    assert.equal(target.querySelectorAll('table.diff').length, 1);
    assert.equal(target.querySelectorAll('.question-panel').length, 1);
    assert.equal(target.querySelectorAll('.composer').length, 1);
    assert.equal(target.querySelector('.composer textarea'), composerInput, 'manual composer must stay mounted');
    assert.equal(composerInput.value, 'unsaved range comment');
    assert.equal(store.composer, composer);
    assert.equal(card('left').querySelector('textarea'), editInput, 'local CommentCard edit state must stay mounted');
    assert.equal(editInput.value, 'unsaved old-side edit');
    assert.equal(discussion('LEFT').querySelector('textarea'), replyInput);
    assert.equal(replyInput.value, 'unsaved old-side reply');
    for (const c of comments) assert.equal(target.querySelectorAll(`[data-comment-id="${c.id}"]`).length, 1);
    for (const side of ['LEFT', 'RIGHT'] as const) assert.equal(target.querySelectorAll(`[data-thread-id="published-${side}"]`).length, 1);
    const ids = [...target.querySelectorAll('[id]')].map((element) => element.id);
    assert.equal(new Set(ids).size, ids.length);
    for (const side of ['LEFT', 'RIGHT'] as const) {
      for (const line of side === 'LEFT' ? [1, 2, 3, 4, 10, 11] : [1, 2, 3, 4, 5, 11]) {
        for (const kind of ['line', 'select', 'comment'] as const) assert.ok(document.getElementById(diffElementId(file.path, kind, side, line)));
      }
    }
  }
  assert.equal(button('select', 'LEFT', 2).closest('tr'), button('select', 'RIGHT', 2).closest('tr'), 'replacement lines align in one physical row');
  assert.equal(button('select', 'LEFT', 4).closest('tr'), button('select', 'RIGHT', 5).closest('tr'), 'context aligns despite shifted numbers');
  const paired = button('select', 'LEFT', 2).closest('tr')!;
  assert.equal(paired.querySelectorAll('td[data-side="LEFT"].sel').length, 2);
  assert.equal(paired.querySelectorAll('td[data-side="RIGHT"].sel').length, 0, 'old selection cannot highlight new code');
  const precedingCode = (element: HTMLElement) => {
    let row = element.closest('tr')!.previousElementSibling;
    while (row?.classList.contains('cmt')) row = row.previousElementSibling;
    return row;
  };
  for (const id of ['left', 'right']) assert.equal(precedingCode(card(id)), paired);
  for (const side of ['LEFT', 'RIGHT'] as const) assert.equal(precedingCode(discussion(side)), paired);
  for (const cell of target.querySelectorAll('td.empty')) {
    assert.equal(cell.textContent?.trim(), '');
    assert.equal(cell.querySelector('button'), null);
    assert.equal(cell.hasAttribute('data-row'), false, 'placeholders have no canonical line to select');
    assert.equal(cell.id, '');
  }
  assert.ok(target.querySelectorAll('td.empty').length >= 4, 'both excess additions and deletions need placeholders');
  button('select', 'RIGHT', 2).click();
  button('select', 'RIGHT', 4).dispatchEvent(new MouseEvent('click', { bubbles: true, shiftKey: true }));
  flushSync();
  assert.deepEqual(store.selection, { path: file.path, side: 'RIGHT', start: 2, end: 4, hunk: 0 });
  assert.equal(paired.querySelectorAll('td[data-side="LEFT"].sel').length, 0);
  assert.equal(paired.querySelectorAll('td[data-side="RIGHT"].sel').length, 2);
  target.querySelector<HTMLButtonElement>('.composer button[aria-label="Cancel comment"]')!.click();
  flushSync();
  button('comment', 'RIGHT', 4).click(); flushSync();
  assert.deepEqual(store.composer?.anchor, store.selection, 'new-side comments retain the entire selected range');
});

test('split pointer ranges ignore empty cells, opposite sides and other hunks and cancel on layout change', async (t) => {
  const file = { ...state().files[0]!, patch: '@@ -1,2 +1,3 @@\n-old one\n-old two\n+new one\n+new two\n+new three\n@@ -10 +11 @@\n-old ten\n+new eleven' };
  const { store } = setup(t, state(1, { files: [file], comments: [] }), mountedStore);
  store.diffLayout = 'split'; store.questionDraft = null; store.composer = null;
  store.fileNavigation = null; store.publishedThreads = null; store.busy = {};
  // DOM-first loading is required for Svelte's browser runtime.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const target = document.createElement('div');
  document.body.append(target);
  const panel = mount(FileDiff, { target, props: { file } });
  let hit: Element | null = null;
  const previousHitTest = Object.getOwnPropertyDescriptor(document, 'elementFromPoint');
  Object.defineProperty(document, 'elementFromPoint', { configurable: true, value: () => hit });
  t.after(async () => {
    await unmount(panel); target.remove();
    if (previousHitTest) Object.defineProperty(document, 'elementFromPoint', previousHitTest);
    else Reflect.deleteProperty(document, 'elementFromPoint');
    store.diffLayout = 'unified'; store.questionDraft = null; store.composer = null;
  });
  flushSync();
  const table = target.querySelector('table')!;
  let captured: number | null = null;
  Object.assign(table, {
    setPointerCapture: (id: number) => { captured = id; },
    hasPointerCapture: (id: number) => captured === id,
    releasePointerCapture: () => { captured = null; },
  });
  const pointer = (element: Element, type: string) => {
    const event = new MouseEvent(type, { bubbles: true, button: 0 });
    Object.defineProperties(event, { pointerId: { value: 1 }, isPrimary: { value: true } });
    element.dispatchEvent(event); flushSync();
  };
  const select = (side: 'LEFT' | 'RIGHT', line: number) => document.getElementById(diffElementId(file.path, 'select', side, line))!;
  pointer(select('LEFT', 1), 'pointerdown');
  assert.equal(captured, 1);
  const original = { ...store.selection! };
  for (const invalid of [
    select('RIGHT', 3).closest('tr')!.querySelector('td.empty')!,
    select('RIGHT', 2).closest('tr')!.querySelector('td.code[data-side="RIGHT"]')!,
    select('LEFT', 10),
  ]) {
    hit = invalid; pointer(table, 'pointermove');
    assert.deepEqual(store.selection, original);
  }
  hit = select('LEFT', 2).closest('tr')!.querySelector('td.code[data-side="LEFT"]')!;
  pointer(table, 'pointermove');
  assert.deepEqual(store.selection, { ...original, end: 2 });
  const focus = store.questionFocus;
  store.diffLayout = 'unified'; flushSync();
  assert.equal(captured, null, 'layout changes release pointer capture');
  hit = select('LEFT', 1); pointer(table, 'pointerup');
  assert.equal(store.questionFocus, focus, 'canceled drags cannot complete against the new layout');
  assert.deepEqual(store.selection, { ...original, end: 2 }, 'switching retains the existing range');
  store.diffLayout = 'split'; flushSync();
  pointer(select('RIGHT', 1), 'pointerdown');
  hit = select('RIGHT', 3);
  pointer(table, 'pointermove'); pointer(table, 'pointerup');
  assert.equal(captured, null);
  assert.deepEqual(store.selection, { path: file.path, side: 'RIGHT', start: 1, end: 3, hunk: 0 });
});

test('review routes accept the four PR tabs without an obsolete Files alias', () => {
  const base = 'http://localhost/github/team/repo/pull/1';
  for (const tab of ['description', 'commits', 'diff', 'guide'] as const) {
    assert.deepEqual(readReviewRoute(new URL(`${base}/${tab}?file=src%2Fcode.ts`)), {
      url: 'https://github.com/team/repo/pull/1', tab, file: anchor.path,
    });
  }
  assert.throws(() => readReviewRoute(new URL(`${base}/files`)));
});

for (const tab of ['diff', 'guide'] as const) test(`bookmarked ${tab} reloads current GitHub evidence before loading comments and restores its file`, async (t) => {
  const { store, handlers, requests } = setup(t);
  const fresh = state(2, {
    pullRequest: { ...state().pullRequest!, headSha: 'fresh-head' },
    comments: [{ ...comment('editable'), stale: true }],
    analysis: { ...analysis(), headSha: 'fresh-head', generation: 'fresh-analysis' },
  });
  handlers['POST /pull-request'] = () => fresh;
  window.history.replaceState(null, '', `/github/team/repo/pull/1/${tab}?file=src%2Fcode.ts`);
  const navigation = new ReviewNavigation(store);
  t.after(() => { navigation.dispose(); window.history.replaceState(null, '', '/'); });
  await navigation.start();
  await settle();
  assert.equal(navigation.error, null);
  assert.equal(store.view, 'pr');
  assert.equal(store.prTab, tab);
  assert.equal(store.selectedPath, 'src/code.ts');
  assert.equal(store.fileNavigation?.path, 'src/code.ts', 'restoring a deep link must request a scroll, not just mark a sidebar row');
  assert.equal(store.fileNavigation?.line, undefined);
  assert.equal(window.location.pathname, `/github/team/repo/pull/1/${tab}`);
  assert.equal(new URLSearchParams(window.location.search).get('file'), anchor.path);
  assert.equal(store.pr?.headSha, 'fresh-head');
  assert.equal(store.comments[0]?.stale, true);
  assert.equal(requests.some((r) => r.path === '/state'), false);
  const comments = requests.filter((r) => r.path.startsWith('/review-comments'));
  assert.equal(comments.length, 1);
  assert.equal(new URLSearchParams(comments[0]!.path.split('?')[1]).get('headSha'), 'fresh-head');
  assert.equal(store.publishedThreads?.headSha, 'fresh-head');
});

for (const tab of ['diff', 'guide'] as const) test(`${tab} navigation records views, replaces file selection, and restores the inbox through history`, async (t) => {
  const secondPath = 'file with spaces#and?.go';
  const initial = state(1, { files: [state().files[0]!, { ...state().files[0]!, path: secondPath }] });
  const { store, handlers, requests } = setup(t, initial);
  handlers['GET /state'] = () => initial;
  window.history.replaceState(null, '', '/');
  const navigation = new ReviewNavigation(store);
  t.after(() => { navigation.dispose(); window.history.replaceState(null, '', '/'); });
  await navigation.start();
  store.clickLine(anchor.path, 'RIGHT', 2, 0, false);
  store.questionDraft!.question = 'keep this question';
  store.openComposer(anchor, diffElementId(anchor.path, 'comment', 'RIGHT', 2));
  store.composer!.body = 'keep this comment';
  const question = store.questionDraft;
  const composer = store.composer;
  const questionFocus = store.questionFocus;
  const composerFocus = store.composerFocus;
  store.resumePr(tab);
  store.selectedPath = 'src/code.ts';
  navigation.sync();
  assert.equal(window.location.pathname, `/github/team/repo/pull/1/${tab}`);
  const length = window.history.length;
  assert.equal(store.fileNavigation, null, 'passive selection cannot create explicit jump intent');
  store.selectFile(anchor.path, { line: { side: 'RIGHT', line: 2 } });
  const explicit = store.fileNavigation;
  store.selectedPath = secondPath;
  navigation.sync();
  assert.equal(window.history.length, length);
  assert.equal(readReviewRoute(new URL(window.location.href))?.file, secondPath);
  assert.equal(store.prTab, tab);
  assert.equal(readReviewRoute(new URL(window.location.href))?.tab, tab);
  assert.equal(store.fileNavigation, explicit, 'URL synchronization must not replay a passive change as a jump');
  assert.equal(store.questionDraft, question);
  assert.equal(store.composer, composer);
  assert.equal(store.questionDraft!.anchor.path, anchor.path);
  assert.equal(store.composer!.anchor.path, anchor.path);
  assert.equal(store.questionFocus, questionFocus);
  assert.equal(store.composerFocus, composerFocus);
  const requestCount = requests.length;
  const replace = t.mock.method(window.history, 'replaceState');
  navigation.sync();
  assert.equal(replace.mock.callCount(), 0, 'syncing the canonical URL is inert');
  assert.equal(requests.length, requestCount);
  store.prTab = 'description';
  navigation.sync();
  assert.equal(window.location.pathname, '/github/team/repo/pull/1/description');
  window.history.replaceState(null, '', '/');
  window.dispatchEvent(new dom.window.PopStateEvent('popstate'));
  await settle();
  assert.equal(store.view, 'home');
  assert.equal(window.location.pathname, '/');
});

for (const tab of ['diff', 'guide'] as const) test(`${tab} history file targets request fresh jumps while untargeted and non-code views stay unforced`, async (t) => {
  const secondPath = 'src/a "quoted"#?:|%2F.ts';
  const initial = state(1, { files: [state().files[0]!, { ...state().files[0]!, path: secondPath }] });
  const { store, handlers, requests } = setup(t, initial);
  handlers['POST /pull-request'] = () => initial;
  const base = '/github/team/repo/pull/1';
  window.history.replaceState(null, '', `${base}/${tab}`);
  const navigation = new ReviewNavigation(store);
  t.after(() => { navigation.dispose(); window.history.replaceState(null, '', '/'); });
  await navigation.start();
  assert.equal(store.prTab, tab);
  assert.equal(store.selectedPath, null);
  assert.equal(store.fileNavigation, null);
  assert.equal(window.location.search, '', 'opening a review does not manufacture a first-file deep link');
  const targeted = `${base}/${tab}?${new URLSearchParams({ file: secondPath })}`;
  const restoreTarget = async (path: string) => {
    window.history.replaceState(null, '', path);
    await navigation.restore();
    navigation.sync();
  };
  const historyLength = window.history.length;
  await restoreTarget(targeted);
  assert.equal(store.prTab, tab);
  const first = store.fileNavigation!;
  assert.equal(first.path, secondPath);
  assert.equal(store.selectedPath, secondPath);
  await restoreTarget(targeted);
  assert.ok(store.fileNavigation!.nonce > first.nonce, 'Back/Forward to the same file still expresses a fresh jump');
  assert.equal(window.history.length, historyLength);
  assert.equal(requests.filter((request) => request.path === '/pull-request').length, 1, 'same-PR history must not reload evidence');
  await restoreTarget(`${base}/${tab}?file=missing.ts`);
  assert.equal(store.prTab, tab);
  assert.equal(store.fileNavigation, null);
  assert.equal(store.selectedPath, null);
  assert.equal(window.location.search, '');
  for (const tab of ['description', 'commits']) {
    await restoreTarget(`${base}/${tab}?${new URLSearchParams({ file: secondPath })}`);
    assert.equal(store.prTab, tab);
    assert.equal(store.fileNavigation, null);
    assert.equal(store.selectedPath, null, 'a file query outside Diff or Guide is not a file navigation');
    assert.equal(window.location.pathname, `${base}/${tab}`);
    assert.equal(window.location.search, '');
  }
  await restoreTarget(targeted);
  await restoreTarget(`${base}/${tab}`);
  assert.equal(store.prTab, tab);
  assert.equal(store.fileNavigation, null);
  assert.equal(store.selectedPath, null);
});

test('rapid history navigation applies the newest target instead of an older PR response', async (t) => {
  const { store, handlers } = setup(t);
  handlers['GET /state'] = () => state();
  window.history.replaceState(null, '', '/');
  const navigation = new ReviewNavigation(store);
  t.after(() => { navigation.dispose(); window.history.replaceState(null, '', '/'); });
  await navigation.start();
  const loading = deferred<AppState>();
  handlers['POST /pull-request'] = () => loading.promise;
  window.history.replaceState(null, '', '/github/team/repo/pull/2/diff');
  const opening = navigation.restore();
  window.history.replaceState(null, '', '/');
  void navigation.restore();
  loading.resolve(state(3, { pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' } }));
  await opening;
  navigation.sync();
  assert.equal(store.view, 'home');
  assert.equal(window.location.pathname, '/');
  assert.equal(navigation.error, null);
});

test('a failed deep link keeps its URL and can retry instead of silently returning to the inbox', async (t) => {
  const { store, handlers } = setup(t);
  handlers['POST /pull-request'] = () => { throw new Error('GitHub temporarily unavailable'); };
  window.history.replaceState(null, '', '/github/team/repo/pull/2/diff');
  const navigation = new ReviewNavigation(store);
  t.after(() => { navigation.dispose(); window.history.replaceState(null, '', '/'); });
  await navigation.start();
  navigation.sync();
  assert.match(navigation.error!, /GitHub temporarily unavailable/);
  assert.equal(window.location.pathname, '/github/team/repo/pull/2/diff');
  handlers['POST /pull-request'] = () => state(4, { pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' } });
  await navigation.restore(true);
  assert.equal(navigation.error, null);
  assert.equal(store.pr?.number, 2);
  assert.equal(store.view, 'pr');
  assert.equal(store.prTab, 'diff');
});

test('same-second follow-ups replace obsolete running metadata without replaying stale job polls', (t) => {
  const current = accepted('shared', 'running', 8);
  const next = accepted('shared', 'queued', 9);
  next.job.id = 'new-turn';
  next.job.startedAt = current.job.startedAt;
  next.job.updatedAt = current.job.updatedAt;
  const merged = mergeVerificationJobs([current.job], [next.job]);
  assert.equal(merged.length, 1);
  assert.equal(merged[0]?.id, next.job.id);
  assert.equal(mergeVerificationJobs(merged, [current.job])[0]?.id, next.job.id);
  const { store } = setup(t, state(1, { chats: [next.thread] }));
  store.verificationJobs = [current.job, next.job];
  assert.equal(store.verificationForThread('shared')?.id, next.job.id);
  assert.match(store.verificationThreadBlockReason('shared')!, /active check/);
  store.verificationJobs = mergeVerificationJobs(merged, [{ ...next.job, revision: 10, status: 'cancelling' }]);
  assert.equal(store.verificationForThread('shared')?.status, 'cancelling');
});

for (const tab of ['diff', 'guide'] as const) test(`restored ${tab} test-file locations respect the collapse preference`, async (t) => {
  const path = 'charts/octopus-deploy/tests/cluster_shared_test.yaml';
  const initial = state(1, { comments: [], files: [
    { ...state().files[0]!, path, testOnly: { onlyTests: true, confidence: 1, probability: 1 } },
    { ...state().files[0]!, path: 'templates/statefulset.yaml', testOnly: { onlyTests: false, confidence: 1, probability: 0 } },
  ] });
  const { store, handlers } = setup(t, initial, mountedStore);
  store.fileNavigation = null; store.composer = null; store.questionDraft = null;
  handlers['POST /pull-request'] = () => initial;
  window.localStorage.setItem('skim.collapseTests', 'true');
  window.history.replaceState(null, '', `/github/team/repo/pull/1/${tab}?${new URLSearchParams({ file: path })}`);
  const navigation = new ReviewNavigation(store);
  t.after(() => {
    navigation.dispose(); window.history.replaceState(null, '', '/');
    window.localStorage.removeItem('skim.collapseTests');
    store.collapseTests = false; store.fileNavigation = null;
  });
  await navigation.start(); await settle();
  // Browser component modules must load after the test DOM above is installed.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const target = document.createElement('div'); document.body.append(target);
  const components = store.files.map((file) => mount(FileDiff, { target, props: { file } }));
  t.after(async () => { for (const component of components) await unmount(component); target.remove(); });
  flushSync();
  const testBody = document.getElementById(diffElementId(path, 'file'))!.querySelector<HTMLElement>('.filebody')!;
  const runtimeBody = document.getElementById(diffElementId('templates/statefulset.yaml', 'file'))!.querySelector<HTMLElement>('.filebody')!;
  assert.equal(store.selectedPath, path);
  assert.equal(testBody.hidden, true, 'a bookmark must not permanently force a test file open');
  assert.equal(runtimeBody.hidden, false, 'production changes remain visible');
  store.selectFile(path, { line: { side: 'RIGHT', line: 1 } }); flushSync();
  assert.equal(testBody.hidden, false, 'an explicit code link still reveals its target');
});

test('test collapse defaults persist while explicit file, unit and editor intent stay distinct', async (t) => {
  const files = [{ ...state().files[0]!, path: 'calc_test.go', testOnly: { onlyTests: true, confidence: 0.99, probability: 0.99 } }, { ...state().files[0]!, path: 'calc.go' }];
  const initial = state(1, { files, groups: [{ id: 'test-unit', title: 'Tests', paths: ['calc_test.go'] }, { id: 'implementation', title: 'Implementation', paths: ['calc.go'] }], comments: [] });
  const { store, handlers } = setup(t, initial, mountedStore);
  store.fileNavigation = null; store.composer = null; store.questionDraft = null;
  store.setCollapseTests(true);
  // Components need the browser DOM installed above before module evaluation.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const target = document.createElement('div'); document.body.append(target);
  const components = files.map((file) => mount(FileDiff, { target, props: { file } }));
  t.after(async () => {
    for (const component of components) await unmount(component);
    target.remove(); store.collapseTests = false; store.composer = null; store.fileNavigation = null;
    window.localStorage.removeItem('skim.collapseTests');
  });
  flushSync();
  const testCard = document.getElementById(diffElementId('calc_test.go', 'file'))!;
  const codeCard = document.getElementById(diffElementId('calc.go', 'file'))!;
  const testBody = testCard.querySelector<HTMLElement>('.filebody')!;
  const codeBody = codeCard.querySelector<HTMLElement>('.filebody')!;
  assert.equal(testBody.hidden, true);
  assert.equal(codeBody.hidden, false);
  store.selectUnit('test-unit', { snapshot: `${identity.prKey}@base..head`, generation: 'generation-1' });
  flushSync();
  assert.equal(store.fileNavigation?.unitId, 'test-unit');
  assert.equal(testBody.hidden, true, 'a unit explanation link does not open its tests');
  store.selectFile('calc_test.go', { line: { side: 'RIGHT', line: 1 } }); flushSync();
  assert.equal(testBody.hidden, false);
  codeCard.querySelector<HTMLButtonElement>('.filehead button[aria-expanded]')!.click(); flushSync();
  store.setCollapseTests(false); flushSync();
  assert.equal(codeBody.hidden, true, 'manual collapse is independent of the test default');
  store.setCollapseTests(true); flushSync();
  assert.equal(testBody.hidden, true, 'turning test collapsing back on folds previously opened tests');
  handlers['GET /state'] = () => initial;
  const reopened = new Store();
  await reopened.boot();
  assert.equal(reopened.collapseTests, true, 'the saved option survives a fresh store boot');
  reopened.dispose();
});

test('unit navigation rejects stale links and clears an obsolete analysis target', async (t) => {
  const initial = state(1, { groups: [{ id: 'unit', title: 'Unit', paths: [anchor.path] }] });
  const { store, handlers } = setup(t, initial);
  const expected = { snapshot: `${identity.prKey}@base..head`, generation: 'generation-1' };
  store.prTab = 'description';
  store.mobileView = 'panel';
  store.settingsOpen = true;
  store.selectUnit('unit', { ...expected, snapshot: `${identity.prKey}@base..obsolete` });
  assert.equal(store.fileNavigation, null);
  store.selectUnit('unit', { ...expected, generation: 'obsolete' });
  assert.equal(store.fileNavigation, null);
  store.selectUnit('unknown', expected);
  assert.equal(store.fileNavigation, null);
  assert.equal(store.view, 'home');
  assert.equal(store.prTab, 'description');
  assert.equal(store.mobileView, 'panel');
  assert.equal(store.settingsOpen, true);
  store.selectUnit('unit', expected);
  const first = store.fileNavigation!;
  assert.equal(store.prTab, 'guide');
  assert.equal(store.mobileView, 'diff');
  store.selectFile(anchor.path, { line: { side: 'RIGHT', line: 2 } });
  assert.equal(store.prTab, 'guide');
  assert.equal(store.fileNavigation?.path, anchor.path);
  assert.equal(store.fileNavigation?.unitId, undefined);
  store.selectUnit('unit', expected);
  assert.ok(store.fileNavigation!.nonce > first.nonce);
  handlers['POST /analysis'] = () => ({ analysis: analysis('generation-2'), files: [{ path: anchor.path }], groups: initial.groups });
  await store.refreshAnalysis();
  assert.equal(store.fileNavigation, null);
});

test('review progress batches retain newer clocks and failed saves do not change reviewed files', async (t) => {
  const files = [state().files[0]!, { ...state().files[0]!, path: 'second.ts' }];
  const { store, handlers, requests } = setup(t, state(1, { files }));
  const first = deferred<FileReviewUpdate>(), second = deferred<FileReviewUpdate>();
  handlers['POST /reviewed'] = (body) => {
    assert.ok(body && typeof body === 'object' && 'paths' in body && Array.isArray(body.paths));
    return body.paths[0] === anchor.path ? first.promise : second.promise;
  };
  const markFirst = store.setReviewed([anchor.path], true);
  assert.equal(store.reviewedPaths.has(anchor.path), false, 'pending writes are not represented as saved progress');
  assert.equal(await store.setReviewed([anchor.path, 'second.ts'], true), false, 'overlapping batches wait');
  const markSecond = store.setReviewed(['second.ts'], true);
  assert.equal(requests.filter((r) => r.path === '/reviewed').length, 2);
  second.resolve({ ...identity, fileReview: { revision: 2, reviewedPaths: [anchor.path, 'second.ts'] } });
  await markSecond;
  first.resolve({ ...identity, fileReview: { revision: 1, reviewedPaths: [anchor.path] } });
  await markFirst;
  assert.deepEqual([...store.reviewedPaths], [anchor.path, 'second.ts']);
  assert.equal(store.state!.revision, 1);
  assert.deepEqual(store.reviewPending, {});
  const stale = state(3, { files, fileReview: { revision: 1, reviewedPaths: [] } });
  assert.equal(mergeState(store.state, stale).fileReview?.revision, 2);
  handlers['POST /reviewed'] = () => { throw new Error('Disk is full'); };
  assert.equal(await store.setReviewed([anchor.path], false), false);
  assert.equal(store.reviewedPaths.has(anchor.path), true);
  assert.match(store.reviewError!, /Disk is full/);
  assert.deepEqual(store.reviewPending, {});
});

test('review progress completion cannot affect a switched PR or its newer pending write', async (t) => {
  const { store, handlers } = setup(t);
  const first = deferred<FileReviewUpdate>(), second = deferred<FileReviewUpdate>();
  handlers['POST /reviewed'] = () => first.promise;
  const oldWrite = store.setReviewed([anchor.path], true);
  const next = state(3, { pullRequest: { ...state().pullRequest!, number: 2, url: 'https://github.com/team/repo/pull/2' } });
  handlers['GET /state'] = () => next;
  await store.boot();
  handlers['POST /reviewed'] = () => second.promise;
  const newWrite = store.setReviewed([anchor.path], true);
  first.resolve({ ...identity, fileReview: { revision: 10, reviewedPaths: [anchor.path] } });
  assert.equal(await oldWrite, false);
  assert.equal(store.reviewPending[anchor.path], true);
  assert.equal(store.reviewedPaths.size, 0);
  second.resolve({ ...identity, prKey: 'github/team/repo/2', fileReview: { revision: 1, reviewedPaths: [anchor.path] } });
  assert.equal(await newWrite, true);
  assert.equal(store.reviewedPaths.has(anchor.path), true);
});

test('file and unit checkboxes share saved state while reviewed, test-default and explicit expansion differ', async (t) => {
  const files = [{ ...state().files[0]!, path: 'value.go' }, { ...state().files[0]!, path: 'value_test.go', testOnly: { onlyTests: true, confidence: 0.99, probability: 0.99 } }];
  const initial = state(1, { files, comments: [] });
  const { store, handlers } = setup(t, initial, mountedStore);
  handlers['GET /state'] = () => initial;
  await store.boot();
  store.collapseTests = true; store.fileNavigation = null; store.composer = null; store.questionDraft = null;
  store.reviewPending = {}; store.reviewError = null;
  const saved = new Set<string>(); let revision = 0;
  handlers['POST /reviewed'] = (body) => {
    assert.ok(body && typeof body === 'object' && 'paths' in body && Array.isArray(body.paths) && 'reviewed' in body && typeof body.reviewed === 'boolean');
    for (const path of body.paths) { assert.ok(typeof path === 'string'); if (body.reviewed) saved.add(path); else saved.delete(path); }
    return { ...identity, fileReview: { revision: ++revision, reviewedPaths: [...saved] } };
  };
  // Mount browser components only after the DOM-first initialization boundary.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const { default: ReviewedCheckbox } = await import('../src/components/ReviewedCheckbox.svelte');
  const target = document.createElement('div'); document.body.append(target);
  const components = files.map((file) => mount(FileDiff, { target, props: { file } }));
  const unitTarget = document.createElement('div'); target.append(unitTarget);
  const unitComponent = mount(ReviewedCheckbox, { target: unitTarget, props: { paths: files.map((file) => file.path) } });
  t.after(async () => {
    await unmount(unitComponent); for (const component of components) await unmount(component);
    target.remove(); store.collapseTests = false; store.fileNavigation = null;
  });
  flushSync();
  const code = document.getElementById(diffElementId('value.go', 'file'))!;
  const tests = document.getElementById(diffElementId('value_test.go', 'file'))!;
  const fileCheckbox = (card: Element) => card.querySelector<HTMLButtonElement>('.reviewed-control [role="checkbox"]')!;
  const unit = unitTarget.querySelector<HTMLButtonElement>('[role="checkbox"]')!;
  assert.equal(tests.querySelector<HTMLElement>('.filebody')!.hidden, true);
  assert.equal(fileCheckbox(tests).getAttribute('aria-checked'), 'false', 'collapsed tests have not been reviewed');
  fileCheckbox(code).click(); flushSync(); await settle(); flushSync();
  assert.equal(code.querySelector<HTMLElement>('.filebody')!.hidden, true);
  assert.equal(fileCheckbox(code).getAttribute('aria-checked'), 'true');
  assert.equal(unit.getAttribute('aria-checked'), 'mixed');
  unit.click(); flushSync(); await settle(); flushSync();
  assert.equal(unit.getAttribute('aria-checked'), 'true');
  assert.equal(fileCheckbox(tests).getAttribute('aria-checked'), 'true');
  store.selectFile('value_test.go'); flushSync();
  assert.equal(tests.querySelector<HTMLElement>('.filebody')!.hidden, false);
  assert.equal(fileCheckbox(tests).getAttribute('aria-checked'), 'true', 'reopening does not erase reviewed status');
  fileCheckbox(tests).click(); flushSync(); await settle(); flushSync();
  assert.equal(fileCheckbox(tests).getAttribute('aria-checked'), 'false');
  assert.equal(tests.querySelector<HTMLElement>('.filebody')!.hidden, false, 'unreviewing expands even with the test default');
  assert.equal(unit.getAttribute('aria-checked'), 'mixed');
});

test('enabling test collapsing preserves selected code and active comment editors', async (t) => {
  const file = { ...state().files[0]!, testOnly: { onlyTests: true, confidence: 1, probability: 1 } };
  const { store } = setup(t, state(1, { files: [file] }), mountedStore);
  store.collapseTests = false; store.fileNavigation = null; store.composer = null; store.questionDraft = null;
  // Static component imports would initialize Svelte before the test DOM exists.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const target = document.createElement('div'); document.body.append(target);
  const component = mount(FileDiff, { target, props: { file } });
  t.after(async () => {
    await unmount(component); target.remove();
    store.collapseTests = false; store.composer = null; store.questionDraft = null;
    window.localStorage.removeItem('skim.collapseTests');
  });
  flushSync();
  const body = target.querySelector<HTMLElement>('.filebody')!;
  target.querySelector<HTMLButtonElement>('button[aria-label="Edit comment"]')!.click(); flushSync();
  const edit = target.querySelector<HTMLTextAreaElement>('[data-comment-id="editable"] textarea')!;
  edit.value = 'Keep this unsaved edit'; edit.dispatchEvent(new Event('input', { bubbles: true })); flushSync();
  store.setCollapseTests(true); flushSync();
  assert.equal(body.hidden, false, 'an existing comment editor must remain visible');
  assert.equal(edit.value, 'Keep this unsaved edit');
  [...target.querySelectorAll<HTMLButtonElement>('[data-comment-id="editable"] button')].find((button) => button.textContent?.trim() === 'Cancel')!.click(); flushSync();
  store.setCollapseTests(false); flushSync();
  store.openComposer(anchor, diffElementId(file.path, 'comment', 'RIGHT', 2));
  store.composer!.body = 'Keep this pending comment'; flushSync();
  store.setCollapseTests(true); flushSync();
  assert.equal(body.hidden, false, 'an inline composer must remain visible');
  assert.equal(target.querySelector<HTMLTextAreaElement>('#inline-body')!.value, 'Keep this pending comment');
  store.composer = null; store.setCollapseTests(false); flushSync();
  document.getElementById(diffElementId(file.path, 'select', 'RIGHT', 1))!.click(); flushSync();
  store.setCollapseTests(true); flushSync();
  assert.equal(body.hidden, false, 'a selected code range must remain visible');
  assert.equal(store.selection?.start, 1);
});

test('late Jev classification collapses untouched test-only changes, not filenames or active selections', async (t) => {
  const paths = ['src/selected.rs', 'src/embedded.rs', 'tests/runtime.test.ts'];
  const initial = state(1, { files: paths.map((path) => ({ ...state().files[0]!, path })), comments: [] });
  const { store, handlers } = setup(t, initial, mountedStore);
  handlers['GET /state'] = () => initial;
  await store.boot();
  store.collapseTests = true; store.fileNavigation = null; store.composer = null; store.questionDraft = null;
  // DOM-first imports exercise actual mounted component behavior.
  const { mount, unmount, flushSync } = await import('./mount-ui');
  const { default: FileDiff } = await import('../src/components/FileDiff.svelte');
  const target = document.createElement('div'); document.body.append(target);
  const components = store.files.map((file) => mount(FileDiff, { target, props: { file } }));
  t.after(async () => {
    for (const component of components) await unmount(component);
    target.remove(); store.collapseTests = false; store.questionDraft = null;
  });
  flushSync();
  const body = (path: string) => document.getElementById(diffElementId(path, 'file'))!.querySelector<HTMLElement>('.filebody')!;
  assert.ok(paths.every((path) => !body(path).hidden), 'unclassified changes stay open even under tests/');
  document.getElementById(diffElementId(paths[0]!, 'select', 'RIGHT', 1))!.click(); flushSync();
  store.files[0]!.testOnly = { onlyTests: true, confidence: 0.98, probability: 0.98 };
  store.files[1]!.testOnly = { onlyTests: true, confidence: 0.98, probability: 0.98 };
  store.files[2]!.testOnly = { onlyTests: false, confidence: 0.99, probability: 0.01 };
  flushSync();
  assert.equal(body(paths[0]!).hidden, false, 'an active code selection overrides a late default');
  assert.equal(body(paths[1]!).hidden, true, 'test-only changes in an ordinary source filename collapse');
  assert.equal(body(paths[2]!).hidden, false, 'mixed changes in a test filename stay open');
  const latest = state(2, { files: [...store.files], analysis: analysis('generation-1', 9), fileReview: { revision: 3, reviewedPaths: [paths[0]!] } });
  const older = state(3, { files: initial.files.map((file) => ({ ...file, testOnly: undefined })), analysis: analysis('generation-1', 2) });
  const merged = mergeState(latest, older);
  assert.equal(merged.files[1]!.testOnly?.probability, 0.98);
  assert.deepEqual(merged.fileReview?.reviewedPaths, [paths[0]!]);
});
