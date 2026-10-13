import {
  api,
  ApiError,
  errorMessage,
  isLiveComment,
  type AnalysisUpdate,
  type AppState,
  type PR,
  type Permission,
  type ReviewComment,
  type ReviewEvent,
  type Settings,
  type SettingsSave,
  type CommitList,
  type ReviewInbox,
  type Workspace,
  type WorkspaceUpdate,
  type Side,
  type ReviewProgress,
  type PublishedThreads,
  type CommitRequest,
  type VerificationJob,
  type VerificationUpdate,
} from './api';
import { conflictReason, mergeEntities, mergeFileReview, mergeState, mergeVerificationJobs, snapshotKey, verificationActive, type Busy } from './concurrency';
import { diffElementId } from './diff';
import { validatedUnits } from './tree';
import { attachmentId } from './attachments';
export type { Busy } from './concurrency';

export type Selection = { path: string; side: Side; start: number; end: number; hunk: number };
type AnchoredDraft = {
  anchor: Selection;
  prKey: string;
  baseSha: string;
  headSha: string;
  returnFocus: string;
};
export type Composer = AnchoredDraft & { body: string };
export type QuestionDraft = AnchoredDraft & { question: string };
type ReplyDraft = CommitRequest & { text: string; pending: boolean; error?: string; uncertain: boolean; refreshed: boolean };
type RequestContext = { epoch: number; snapshot: string; analysisVersion: number; metadataVersion: number };
export type Tab = 'chat' | 'verify' | 'review' | 'comments';
export type View = 'home' | 'pr';
export type PrTab = 'description' | 'commits' | 'diff' | 'guide';
export type CommitsState = { status: 'loading' | 'ready' | 'error'; list?: CommitList; error?: string };
export type InboxFilter = { username: string; repo: string };

const prKeyOf = (pr: PR) => `${pr.provider}/${pr.owner}/${pr.repo}/${pr.number}`;
/** Commit cache identity: the exact PR key plus both loaded SHAs. */
export const commitKey = (pr: PR) => `${prKeyOf(pr)}@${pr.baseSha}..${pr.headSha}`;

export class Store {
  state = $state<AppState | null>(null);
  booting = $state(true);
  busy = $state<Partial<Record<Busy, boolean>>>({});
  errors = $state<Partial<Record<Busy | 'comment' | 'permission' | 'workspace', string>>>({});
  notice = $state<string | null>(null);
  permissions = $state<Permission[]>([]);
  permissionErrors = $state<Record<string, string | undefined>>({});
  permissionSubmitting = $state<Record<string, boolean>>({});
  verificationJobs = $state<VerificationJob[]>([]);
  verificationErrors = $state<Record<string, string | undefined>>({});
  verificationThreadErrors = $state<Record<string, string | undefined>>({});
  verificationPollingError = $state<string | null>(null);
  verificationCancelling = $state<Record<string, boolean>>({});
  activityOpen = $state(false);
  activityClearing = $state(false);
  activityClearError = $state<string | null>(null);
  lastAcceptedJobId = $state<string | null>(null);
  #answeredPermissions = new Set<string>();
  #verificationAbort: AbortController | null = null;

  selectedPath = $state<string | null>(null);
  /** Explicit jump intent; restored file locations scroll without forcing the file open. */
  fileNavigation = $state<{ path: string; expand?: boolean; line?: { side: Side; line: number }; unitId?: string; snapshot?: string; generation?: string; nonce: number } | null>(null);
  #fileNavigationNonce = 0;
  questionDraft = $state<QuestionDraft | null>(null);
  questionFocus = $state(0);
  composer = $state<Composer | null>(null);
  composerFocus = $state(0);
  composerNote = $state<string | null>(null);
  tab = $state<Tab>('chat');
  mobileView = $state<'files' | 'diff' | 'panel'>('diff');
  /** Top-level context. Home and the PR workspace stay mounted; Settings overlays whichever is current. */
  view = $state<View>('home');
  prTab = $state<PrTab>('guide');
  settingsOpen = $state(false);
  reviewEvent = $state<ReviewEvent>('COMMENT');
  finishReviewContext = $state<PR | null>(null);
  reviewProgress = $state<ReviewProgress | null>(null);
  reviewProgressError = $state<string | null>(null);
  reviewedPaths: ReadonlySet<string> = $derived(new Set(this.state?.fileReview?.reviewedPaths ?? []));
  reviewPending = $state<Record<string, boolean>>({});
  reviewError = $state<string | null>(null);
  publishedThreads = $state<PublishedThreads | null>(null);
  publishedThreadsLoading = $state(false);
  publishedThreadsError = $state<string | null>(null);
  replyDrafts = $state<Record<string, ReplyDraft>>({});
  #reviewTimer: number | null = null;
  #reviewAbort: AbortController | null = null;
  #reviewEpoch = 0;
  #threadsAbort: AbortController | null = null;
  #threadsEpoch = 0;
  #disposed = false;
  #contextEpoch = 0;
  #metadataVersion = 0;
  #permissionEpoch = 0;

  inbox = $state<ReviewInbox | null>(null);
  inboxLoading = $state(false);
  inboxError = $state<string | null>(null);
  /** Filters applied to the last/in-flight request; the Home inputs hold uncommitted edits separately. */
  inboxFilter = $state<InboxFilter>({ username: '', repo: '' });
  commitStates = $state<Record<string, CommitsState>>({});
  /** Pre-signed GitHub image URLs for one exact snapshot (see loadAttachments). */
  attachments = $state<{ key: string; urls: Record<string, string> } | null>(null);
  #attachmentsLoading = false;
  #attachmentsFetchedAt = 0;
  #inboxEpoch = 0;
  #inboxAbort: AbortController | null = null;

  activeChatId = $state<string | null>(null);
  activeVerifyId = $state<string | null>(null);
  conversationDrafts = $state<Record<string, string>>({});
  get chatDraftKey() { return `${snapshotKey(this.pr)}/chat/${this.activeChatId ?? 'new'}`; }
  get verifyDraftKey() { return `${snapshotKey(this.pr)}/verify/${this.activeVerifyId ?? 'new'}`; }
  get chatInput() { return this.conversationDrafts[this.chatDraftKey] ?? ''; }
  set chatInput(text: string) { this.conversationDrafts[this.chatDraftKey] = text; }
  get verifyInput() { return this.conversationDrafts[this.verifyDraftKey] ?? ''; }
  set verifyInput(text: string) { this.conversationDrafts[this.verifyDraftKey] = text; }
  selectionNote = $state<string | null>(null);

  analysisError = $state<string | null>(null);
  analysisRefreshing = $state(false);
  /** Reading preferences are independent of the visible file and survive in-app navigation. */
  diffLayout = $state<'unified' | 'split'>('unified');
  collapseTests = $state(false);

  #workspaceTimer: number | null = null;
  #workspaceAbort: AbortController | null = null;
  #workspaceEpoch = 0;
  #polling = false;
  #poll: ReturnType<typeof setInterval> | null = null;
  #analysisTimer: number | null = null;
  #analysisAbort: AbortController | null = null;
  #analysisEpoch = 0;
  /** Changes only when analysis context/generation changes, never for unrelated completions. */
  #analysisVersion = 0;

  get pr() {
    return this.state?.pullRequest ?? null;
  }
  get files() {
    return this.state?.files ?? [];
  }
  get comments() {
    return (this.state?.comments ?? []).filter(isLiveComment);
  }
  get pendingComments() {
    return this.comments.filter((comment) => comment.status === 'pending');
  }
  get publicationBlockReason(): string | null {
    const blocked = this.blockReason('publish');
    if (blocked) return blocked;
    if (!this.pr) return 'Load a pull request before submitting a review.';
    if (this.pendingComments.some((comment) => comment.stale || comment.headSha !== this.pr!.headSha)) {
      return 'Pending comments belong to earlier code. Delete those comments and recreate them on the current diff before submitting.';
    }
    return null;
  }
  get finishReviewStale() {
    return !!this.finishReviewContext && snapshotKey(this.finishReviewContext) !== snapshotKey(this.pr);
  }
  get globalThreads() {
    return (this.state?.chats ?? []).filter((c) => c.kind === 'global');
  }
  get verifyThreads() {
    return (this.state?.chats ?? []).filter((c) => c.kind === 'verification');
  }
  get activeVerificationJobs() {
    return this.verificationJobs.filter(verificationActive);
  }
  get prVerificationJobs() {
    const key = this.pr && prKeyOf(this.pr);
    return this.activeVerificationJobs.filter((job) => job.prKey === key);
  }
  verificationForThread(threadId: string) {
    const scoped = this.verifyThreads.find((thread) => thread.id === threadId)?.verification;
    if (scoped) return this.verificationJobs.find((job) => job.id === scoped.id) ?? scoped;
    const prKey = this.pr && prKeyOf(this.pr);
    return this.verificationJobs.find((job) => job.threadId === threadId && job.prKey === prKey);
  }
  verificationThreadBlockReason(threadId: string): string | null {
    const job = this.verificationForThread(threadId);
    if (job && verificationActive(job)) return 'This thread already has an active check. Wait or cancel it before sending a follow-up; new checks can run independently.';
    if (this.verifyThreads.find((thread) => thread.id === threadId)?.stale) return 'This earlier-code thread cannot be continued. Select current code to start a new check.';
    return this.verificationBlockReason;
  }
  /** Server-owned managed checkout for the loaded PR; absent until the backend reports one. */
  get workspace(): Workspace | null {
    return this.state?.workspace ?? null;
  }
  /** Set when the PR is preparing, in use, or cleaning up; a single in-flight request per tick. */
  get workspaceActivity(): 'preparing' | 'verifying' | 'cleaning' | null {
    const st = this.workspace?.status;
    if (this.busy.finish || this.busy.publish || st === 'cleanup_pending') return 'cleaning';
    if (st === 'preparing') return 'preparing';
    // A server-held lease counts as activity even without our own request (restart, orphan process, another window).
    if (st === 'in_use') return 'verifying';
    if (this.prVerificationJobs.length) return 'verifying';
    return null;
  }
  blockReason(action: Busy): string | null {
    if (this.booting) return 'Skim is still loading the review.';
    return conflictReason(action, {
      busy: { ...this.busy, refresh: this.analysisRefreshing },
      reviewRunning: this.reviewProgress?.status === 'running',
      reviewUnavailable: !this.busy.review && !!this.reviewProgressError,
      verificationRunning: this.prVerificationJobs.length,
      verificationPermissionPending: this.permissions.some((permission) => permission.prKey === (this.pr && prKeyOf(this.pr))),
      workspaceStatus: this.workspace?.status,
    });
  }
  get verificationBlockReason(): string | null {
    if (this.booting) return 'Skim is still loading the review.';
    if (!this.pr) return 'Load a pull request before asking the agent.';
    const blocked = this.blockReason('verify');
    if (blocked) return blocked;
    if (!this.state?.settings.acp.command.trim()) return 'Choose a verification agent in Settings before asking about selected code.';
    return null;
  }
  openFinishReview() {
    if (!this.pr) return;
    this.finishReviewContext = { ...this.pr };
    this.errors.publish = undefined;
  }
  #identity(): CommitRequest | null {
    const pr = this.pr;
    return pr ? { prKey: prKeyOf(pr), baseSha: pr.baseSha, headSha: pr.headSha } : null;
  }
  #matches(r: CommitRequest) {
    const p = this.#identity();
    return !!p && p.prKey === r.prKey && p.baseSha === r.baseSha && p.headSha === r.headSha;
  }
  #capture(): RequestContext {
    return { epoch: this.#contextEpoch, snapshot: snapshotKey(this.pr), analysisVersion: this.#analysisVersion, metadataVersion: this.#metadataVersion };
  }
  #current(request: RequestContext) {
    return !this.#disposed && request.epoch === this.#contextEpoch && request.snapshot === snapshotKey(this.pr);
  }
  async setReviewed(paths: readonly string[], reviewed: boolean): Promise<boolean> {
    const identity = this.#identity();
    if (this.booting || !identity || !paths.length) return false;
    if (paths.some((path) => this.reviewPending[path])) return false;
    const batch = [...paths];
    const currentPaths = new Set(this.files.map((file) => file.path));
    if (new Set(batch).size !== batch.length || batch.some((path) => !currentPaths.has(path))) {
      this.reviewError = 'Review progress can only be saved for files in the current pull request.';
      return false;
    }
    const request = this.#capture();
    this.reviewError = null;
    for (const path of batch) this.reviewPending[path] = true;
    try {
      const update = await api.setReviewed({ ...identity, paths: batch, reviewed });
      if (!this.#current(request) || !this.state) return false;
      if (!this.#matches(update)) {
        this.reviewError = 'The pull request changed before review progress could be saved. Reload it and try again.';
        return false;
      }
      this.state.fileReview = mergeFileReview(this.state.fileReview, update.fileReview);
      return true;
    } catch (error) {
      if (this.#current(request)) this.reviewError = errorMessage(error);
      return false;
    } finally {
      if (this.#current(request)) for (const path of batch) delete this.reviewPending[path];
    }
  }
  #draftStale(draft: AnchoredDraft | null) {
    const pr = this.pr;
    return !!draft && (!pr || draft.prKey !== prKeyOf(pr) || draft.baseSha !== pr.baseSha || draft.headSha !== pr.headSha);
  }
  get composerStale() {
    return this.#draftStale(this.composer);
  }
  get questionStale() {
    return this.#draftStale(this.questionDraft);
  }
  get selection() {
    return this.questionDraft?.anchor ?? null;
  }

  /** Only another manual comment can move the composer; AI selection is independent. */
  openComposer(anchor: Selection, returnFocus: string) {
    const c = this.composer;
    const same = c && !this.composerStale && c.anchor.path === anchor.path && c.anchor.side === anchor.side && c.anchor.hunk === anchor.hunk && c.anchor.start === anchor.start && c.anchor.end === anchor.end;
    if (!same && c && (this.busy.createComment || c.body)) {
      this.composerNote = 'Finish or cancel your open comment before commenting on different lines. Its text and anchor have not moved.';
      this.composerFocus++;
      return;
    }
    if (!this.pr) return;
    if (!same) this.composer = {
      anchor: { ...anchor }, body: '', returnFocus,
      prKey: prKeyOf(this.pr), baseSha: this.pr.baseSha, headSha: this.pr.headSha,
    };
    this.composerNote = null;
    this.errors.createComment = undefined;
    this.composerFocus++;
  }

  clearQuestion() {
    this.questionDraft = null;
    this.selectionNote = null;
    this.errors.verify = undefined;
  }

  async askQuestion() {
    const draft = this.questionDraft;
    if (!draft || this.questionStale || !draft.question.trim()) return false;
    const question = draft.question;
    const saved = await this.verify(question.trim(), draft.anchor);
    if (saved && this.questionDraft === draft && draft.question === question) {
      this.clearQuestion();
      return true;
    }
    return false;
  }

  async createComment() {
    const c = this.composer;
    if (!c || this.composerStale || !c.body.trim()) return false;
    const request = this.#capture();
    const result = await this.#run('createComment', () => api.createComment({
      prKey: c.prKey, baseSha: c.baseSha, headSha: c.headSha,
      path: c.anchor.path, startLine: c.anchor.start, endLine: c.anchor.end, side: c.anchor.side, body: c.body,
    }));
    if (!result || !this.#current(request) || !this.state) return false;
    this.state.comments = mergeEntities(this.state.comments, [result]);
    return true;
  }

  #apply(s: AppState, request: RequestContext, options: { switchContext?: boolean; workspace?: boolean } = {}): boolean {
    if (!this.#current(request)) return false;
    const old = this.state;
    const prevKey = snapshotKey(old?.pullRequest);
    const key = snapshotKey(s.pullRequest);
    if (key !== prevKey && (!options.switchContext || (old && s.revision < old.revision))) return false;
    const next = mergeState(old, s, request.analysisVersion !== this.#analysisVersion, request.metadataVersion !== this.#metadataVersion);
    // Workspace is a separately polled lease, not part of the durable State clock.
    // Only the checkout operation itself (or a context load) can replace it.
    if (key === prevKey) next.workspace = options.workspace ? s.workspace : old?.workspace;
    const analysisChanged = key !== prevKey || next.analysis?.generation !== old?.analysis?.generation;
    this.state = next;
    this.verificationJobs = mergeVerificationJobs(this.verificationJobs, next.chats.flatMap((thread) => thread.verification ? [thread.verification] : []));
    if (analysisChanged) {
      this.#analysisVersion++;
      this.#stopAnalysisPolling();
      if (this.fileNavigation?.unitId !== undefined) this.fileNavigation = null;
    }
    if (key !== prevKey) {
      this.#contextEpoch++;
      this.fileNavigation = null;
      this.selectedPath = null;
      this.activeChatId = null;
      this.activeVerifyId = null;
      this.analysisError = null;
      this.#stopReviewPolling();
      this.reviewProgress = null;
      this.reviewProgressError = null;
      this.reviewPending = {};
      this.reviewError = null;
      this.#threadsEpoch++;
      this.#threadsAbort?.abort();
      this.#threadsAbort = null;
      this.publishedThreads = null;
      this.publishedThreadsError = null;
      void this.refreshReviewProgress();
      void this.loadPublishedThreads();
      this.#verificationAbort?.abort();
      this.#verificationAbort = null;
      void this.pollVerifications();
    }
    if (!next.files.some((f) => f.path === this.selectedPath)) this.selectedPath = null;
    this.#syncAnalysisPolling();
    if (key !== prevKey || options.workspace) this.#stopWorkspacePolling();
    this.#syncWorkspacePolling();
    return true;
  }

  async boot(url?: string): Promise<boolean> {
    this.booting = true;
    this.errors.load = undefined;
    try { this.collapseTests = window.localStorage.getItem('skim.collapseTests') === 'true'; }
    catch { /* Browser storage can be unavailable; the in-memory preference still works. */ }
    try {
      this.#disposed = false;
      const request = this.#capture();
      const previous = snapshotKey(this.pr);
      // A bookmarked review must fetch GitHub first, not restore stale saved refs.
      const state = url ? await api.loadPullRequest(url) : await api.state();
      if (!this.#apply(state, request, { switchContext: true, workspace: true })) return false;
      this.#startPermissionPolling();
      if (url && previous === snapshotKey(this.pr)) void this.loadPublishedThreads(true);
      return true;
    } catch (e) {
      this.errors.load = errorMessage(e);
      return false;
    } finally {
      this.booting = false;
    }
  }

  /** Admit only conflicting actions. Each request owns its own busy flag and polling lifetime. */
  async #run<T>(key: Busy, fn: () => Promise<T>): Promise<T | undefined> {
    const blocked = this.blockReason(key);
    if (blocked) {
      this.errors[key] = blocked;
      return undefined;
    }
    this.busy[key] = true;
    this.errors[key] = undefined;
    this.notice = null;
    const request = this.#capture();
    if (key === 'finish' || key === 'publish') this.#stopWorkspacePolling();
    if (key === 'verify') this.#startPermissionPolling();
    this.#syncWorkspacePolling();
    let failed = false;
    try {
      return await fn();
    } catch (e) {
      failed = true;
      if (this.#current(request)) this.errors[key] = errorMessage(e);
      return undefined;
    } finally {
      this.busy[key] = false;
      this.#syncReviewPolling();
      // A failed action may leave a retained or released checkout the cached status doesn't know about.
      if (failed && this.#current(request) && (key === 'verify' || key === 'finish' || key === 'publish')) void this.#refreshWorkspace();
      else this.#syncWorkspacePolling();
    }
  }

  /** Global activity and permissions remain polled on Home and across PR navigation. */
  #startPermissionPolling() {
    if (this.#poll !== null || this.#disposed) return;
    this.#poll = setInterval(() => {
      void this.pollPermissions();
      void this.pollVerifications();
    }, 1500);
    void this.pollPermissions();
    void this.pollVerifications();
  }
  #stopPermissionPolling() {
    this.#permissionEpoch++;
    clearInterval(this.#poll ?? undefined);
    this.#poll = null;
    this.#verificationAbort?.abort();
    this.#verificationAbort = null;
  }
  async pollPermissions() {
    if (this.#polling || this.#disposed) return;
    this.#polling = true;
    const epoch = this.#permissionEpoch;
    try {
      const { permissions } = await api.permissions();
      if (this.#disposed || epoch !== this.#permissionEpoch) return;
      this.permissions = permissions.filter((permission) => !this.#answeredPermissions.has(permission.id));
      this.errors.permission = undefined;
    } catch (e) {
      if (!this.#disposed && epoch === this.#permissionEpoch) this.errors.permission = `Cannot check for agent permission requests: ${errorMessage(e)}`;
    } finally {
      this.#polling = false;
    }
  }
  async answerPermission(id: string, optionId: string) {
    const permission = this.permissions.find((item) => item.id === id);
    if (!permission?.options.some((option) => option.optionId === optionId) || this.permissionSubmitting[id]) return;
    this.permissionSubmitting[id] = true;
    this.permissionErrors[id] = undefined;
    try {
      await api.resolvePermission(id, optionId);
      this.#answeredPermissions.add(id);
      this.permissions = this.permissions.filter((p) => p.id !== id);
    } catch (e) {
      this.permissionErrors[id] = errorMessage(e);
      void this.pollPermissions();
    } finally {
      this.permissionSubmitting[id] = false;
    }
  }

  async pollVerifications() {
    if (this.#disposed || this.#verificationAbort) return;
    const identity = this.#identity();
    const request = this.#capture();
    const ctrl = new AbortController();
    this.#verificationAbort = ctrl;
    try {
      const update = await api.verifications(identity ?? undefined, ctrl.signal);
      if (this.#disposed || ctrl.signal.aborted) return;
      this.verificationJobs = mergeVerificationJobs(this.verificationJobs, update.jobs);
      if (identity && this.#current(request) && this.#matches(update)) this.#mergeVerifications(update);
      this.verificationPollingError = null;
      this.#syncWorkspacePolling();
    } catch (e) {
      if (!this.#disposed && !ctrl.signal.aborted) this.verificationPollingError = `Cannot refresh checks: ${errorMessage(e)}`;
    } finally {
      if (this.#verificationAbort === ctrl) this.#verificationAbort = null;
    }
  }
  #mergeVerifications(update: VerificationUpdate) {
    if (!this.state || !this.#matches(update)) return;
    this.state.chats = mergeEntities(this.state.chats, update.threads);
    this.state.comments = mergeEntities(this.state.comments, update.comments);
  }
  async cancelVerification(jobId: string) {
    const job = this.verificationJobs.find((item) => item.id === jobId);
    if (!job || !verificationActive(job) || this.verificationCancelling[jobId]) return;
    this.verificationCancelling[jobId] = true;
    this.verificationErrors[jobId] = undefined;
    try {
      const result = await api.cancelVerification(job.id, { prKey: job.prKey, baseSha: job.baseSha, headSha: job.headSha });
      this.verificationJobs = mergeVerificationJobs(this.verificationJobs, [result]);
      void this.pollPermissions();
      void this.pollVerifications();
    } catch (e) {
      this.verificationErrors[jobId] = errorMessage(e);
    } finally {
      this.verificationCancelling[jobId] = false;
    }
  }
  async clearActivity() {
    if (this.activityClearing) return;
    this.activityClearing = true;
    this.activityClearError = null;
    try {
      const { jobs } = await api.clearVerificationActivity();
      this.verificationJobs = mergeVerificationJobs(this.verificationJobs, jobs);
    } catch (e) {
      this.activityClearError = `Cannot clear activity: ${errorMessage(e)}`;
    } finally {
      this.activityClearing = false;
    }
  }
  /** Explicit user intent is the only path from global activity to a different review/thread. */
  async openVerification(jobId: string) {
    const job = this.verificationJobs.find((item) => item.id === jobId);
    if (!job) return;
    this.verificationErrors[jobId] = undefined;
    if (!this.pr || prKeyOf(this.pr) !== job.prKey) {
      if (!await this.openPullRequest(job.prUrl, 'diff')) {
        this.verificationErrors[jobId] = this.errors.load || 'Could not open this check’s pull request.';
        return;
      }
    }
    this.resumePr('diff');
    this.activeVerifyId = job.threadId;
    this.tab = 'verify';
    this.mobileView = 'panel';
    this.activityOpen = false;
    void this.pollVerifications();
  }

  async loadPullRequest(url: string) {
    const request = this.#capture();
    const previous = snapshotKey(this.pr);
    const s = await this.#run('load', () => api.loadPullRequest(url));
    if (!s || !this.#apply(s, request, { switchContext: true, workspace: true })) return false;
    if (previous === snapshotKey(this.pr)) void this.loadPublishedThreads(true);
    return true;
  }
  /** Open a PR by URL (header input or Home row). Navigation happens only after a successful load; Guide is the landing view. */
  async openPullRequest(url: string, tab: PrTab = 'guide') {
    const ok = await this.loadPullRequest(url);
    if (ok) this.resumePr(tab);
    return ok;
  }
  goHome() {
    this.settingsOpen = false;
    this.view = 'home';
  }
  resumePr(tab?: PrTab) {
    if (!this.pr) return;
    this.settingsOpen = false;
    this.view = 'pr';
    if (tab) this.prTab = tab;
    if (tab === 'diff' || tab === 'guide') this.mobileView = 'diff';
  }

  async loadInbox(filter: InboxFilter = this.inboxFilter) {
    this.#inboxAbort?.abort();
    const ctrl = new AbortController();
    this.#inboxAbort = ctrl;
    const epoch = ++this.#inboxEpoch;
    this.inboxFilter = { username: filter.username.trim(), repo: filter.repo.trim() };
    this.inboxLoading = true;
    this.inboxError = null;
    try {
      const r = await api.inbox({ username: this.inboxFilter.username, repo: this.inboxFilter.repo }, ctrl.signal);
      if (epoch !== this.#inboxEpoch) return;
      this.inbox = r;
    } catch (e) {
      if (epoch !== this.#inboxEpoch) return;
      this.inboxError = errorMessage(e);
    } finally {
      if (epoch === this.#inboxEpoch) {
        this.inboxLoading = false;
        this.#inboxAbort = null;
      }
    }
  }

  /** Lazily fetch commits for the loaded PR; results are cached and applied strictly by PR key + base/head identity. */
  async loadCommits(force = false) {
    const pr = this.pr;
    if (!pr) return;
    const key = commitKey(pr);
    const cur = this.commitStates[key];
    if (cur?.status === 'loading' || (cur?.status === 'ready' && !force)) return;
    this.commitStates[key] = { status: 'loading' };
    try {
      const list = await api.commits({ prKey: prKeyOf(pr), baseSha: pr.baseSha, headSha: pr.headSha });
      if (list.prKey !== prKeyOf(pr) || list.baseSha !== pr.baseSha || list.headSha !== pr.headSha) throw new Error('Commit response did not match the requested pull request; reload the PR.');
      this.commitStates[key] = { status: 'ready', list };
    } catch (e) {
      this.commitStates[key] = { status: 'error', error: errorMessage(e) };
    }
  }

  /**
   * Fetch GitHub's pre-signed image URLs for the loaded snapshot. They expire after about five
   * minutes, so images call this again when one fails; repeats within 30s are ignored.
   */
  async loadAttachments() {
    const pr = this.pr;
    if (!pr || this.#attachmentsLoading || (this.attachments?.key === commitKey(pr) && Date.now() - this.#attachmentsFetchedAt < 30_000)) return;
    const key = commitKey(pr);
    this.#attachmentsLoading = true;
    try {
      const result = await api.attachments({ prKey: prKeyOf(pr), baseSha: pr.baseSha, headSha: pr.headSha });
      if (result.prKey === prKeyOf(pr) && result.baseSha === pr.baseSha && result.headSha === pr.headSha && this.pr && commitKey(this.pr) === key) this.attachments = { key, urls: result.urls };
    } catch {
      // Unsigned URLs still render; public attachments load without signing.
    } finally {
      this.#attachmentsLoading = false;
      this.#attachmentsFetchedAt = Date.now();
    }
  }

  /** Signed URL for a GitHub attachment in the current snapshot, if one has been fetched. */
  signedAttachment(raw: string): string | undefined {
    const id = attachmentId(raw);
    return id && this.pr && this.attachments?.key === commitKey(this.pr) ? this.attachments.urls[id] : undefined;
  }

  /** Analysis refresh is independent of AI work; only captured-context changes conflict. */
  async refreshAnalysis() {
    if (!this.pr) return;
    const blocked = this.blockReason('refresh');
    if (blocked) { this.analysisError = blocked; return; }
    this.analysisRefreshing = true;
    this.analysisError = null;
    this.#stopAnalysisPolling();
    const version = this.#analysisVersion;
    try {
      const u = await api.refreshAnalysis();
      if (version === this.#analysisVersion) this.#adoptAnalysis(u);
    } catch (e) {
      if (version === this.#analysisVersion) this.analysisError = errorMessage(e);
    } finally {
      this.analysisRefreshing = false;
      this.#syncAnalysisPolling();
    }
  }

  /** After a settings save the server may have started a new generation; adopt only its analysis fields. */
  async #reacquireAnalysis() {
    const pr = this.state?.pullRequest;
    if (!pr) return;
    this.analysisRefreshing = true;
    this.#stopAnalysisPolling();
    const version = this.#analysisVersion;
    try {
      const st = await api.state();
      const p = st.pullRequest;
      if (version !== this.#analysisVersion || !p || snapshotKey(p) !== snapshotKey(pr) || !st.analysis) return;
      this.#adoptAnalysis({ analysis: st.analysis, files: st.files.map((f) => ({ path: f.path, risk: f.risk, testOnly: f.testOnly })), groups: st.groups });
    } catch (e) {
      if (version === this.#analysisVersion) this.analysisError = errorMessage(e);
    } finally {
      this.analysisRefreshing = false;
      this.#syncAnalysisPolling();
    }
  }

  #adoptAnalysis(u: AnalysisUpdate) {
    if (this.#mergeAnalysis(u, true)) this.#analysisVersion++;
  }

  /**
   * Merge a scoped analysis response, touching only analysis, per-file Jev assessments and groups.
   * Polls must match the current generation and never move revision backwards; `adopt` additionally accepts a newer
   * generation (explicit refresh / fresh state). Either way the PR identity and both SHAs must match.
   */
  #mergeAnalysis(u: AnalysisUpdate, adopt: boolean): boolean {
    const pr = this.state?.pullRequest;
    const a = u.analysis;
    const cur = this.state?.analysis;
    if (!this.state || !pr || !a) return false;
    if (a.key !== `${pr.provider}/${pr.owner}/${pr.repo}/${pr.number}` || a.baseSha !== pr.baseSha || a.headSha !== pr.headSha) return false;
    if (cur && a.generation === cur.generation && a.revision < cur.revision) return false;
    if (!adopt && (!cur || a.generation !== cur.generation)) return false;
    const assessments = new Map(u.files.map((file) => [file.path, file]));
    for (const file of this.state.files) {
      const assessment = assessments.get(file.path);
      file.risk = assessment?.risk;
      file.testOnly = assessment?.testOnly;
    }
    this.state.groups = u.groups;
    this.state.analysis = a;
    const intent = this.fileNavigation;
    if (intent?.unitId !== undefined && (intent.generation !== a.generation || !u.groups.some((group) => group.id === intent.unitId))) this.fileNavigation = null;
    return true;
  }

  #syncAnalysisPolling() {
    const a = this.state?.analysis;
    const active = !!a && (a.risk.status === 'running' || a.grouping.status === 'running');
    if (this.#disposed || !active) this.#stopAnalysisPolling();
    else if (this.#analysisTimer === null && this.#analysisAbort === null) {
      this.#analysisTimer = window.setTimeout(() => void this.#pollAnalysis(), 1000);
    }
  }
  #stopAnalysisPolling() {
    this.#analysisEpoch++;
    clearTimeout(this.#analysisTimer ?? undefined);
    this.#analysisTimer = null;
    this.#analysisAbort?.abort();
    this.#analysisAbort = null;
  }
  async #pollAnalysis() {
    this.#analysisTimer = null;
    const epoch = this.#analysisEpoch;
    const ctrl = new AbortController();
    this.#analysisAbort = ctrl;
    try {
      const u = await api.analysis(ctrl.signal);
      if (epoch !== this.#analysisEpoch) return;
      this.#mergeAnalysis(u, false);
      this.analysisError = null;
    } catch (e) {
      if (epoch !== this.#analysisEpoch) return;
      this.analysisError = errorMessage(e);
    }
    this.#analysisAbort = null;
    this.#syncAnalysisPolling();
  }

  /** Poll only while a checkout is being prepared, used by verification, or cleaned up. */
  #syncWorkspacePolling() {
    if (this.#disposed || !this.pr || !this.workspaceActivity) this.#stopWorkspacePolling();
    else if (this.#workspaceTimer === null && this.#workspaceAbort === null) {
      this.#workspaceTimer = window.setTimeout(() => void this.#pollWorkspace(), 700);
    }
  }
  #stopWorkspacePolling() {
    this.#workspaceEpoch++;
    clearTimeout(this.#workspaceTimer ?? undefined);
    this.#workspaceTimer = null;
    this.#workspaceAbort?.abort();
    this.#workspaceAbort = null;
  }
  /** One final scoped read after a failed action (not a retry of it); supersedes any in-flight poll. */
  async #refreshWorkspace() {
    this.#stopWorkspacePolling();
    const epoch = this.#workspaceEpoch;
    const ctrl = new AbortController();
    this.#workspaceAbort = ctrl;
    try {
      const u = await api.workspace(ctrl.signal);
      if (epoch !== this.#workspaceEpoch) return;
      this.#mergeWorkspace(u);
    } catch (e) {
      if (epoch !== this.#workspaceEpoch) return;
      this.errors.workspace = `Cannot check checkout status: ${errorMessage(e)}`;
    }
    this.#workspaceAbort = null;
    this.#syncWorkspacePolling();
  }
  async #pollWorkspace() {
    this.#workspaceTimer = null;
    const epoch = this.#workspaceEpoch;
    const ctrl = new AbortController();
    this.#workspaceAbort = ctrl;
    try {
      const u = await api.workspace(ctrl.signal);
      if (epoch !== this.#workspaceEpoch) return;
      this.#mergeWorkspace(u);
      this.errors.workspace = undefined;
    } catch (e) {
      if (epoch !== this.#workspaceEpoch) return;
      this.errors.workspace = `Cannot check checkout progress: ${errorMessage(e)}`;
    }
    this.#workspaceAbort = null;
    this.#syncWorkspacePolling();
  }
  /** Touches only state.workspace, and only for the exact PR snapshot the response names. */
  #mergeWorkspace(u: WorkspaceUpdate): boolean {
    const pr = this.state?.pullRequest;
    if (!this.state || !pr || !u.workspace) return false;
    if (u.prKey !== prKeyOf(pr) || u.baseSha !== pr.baseSha || u.headSha !== pr.headSha) return false;
    this.state.workspace = u.workspace;
    return true;
  }

  #stopReviewPolling() {
    this.#reviewEpoch++;
    clearTimeout(this.#reviewTimer ?? undefined);
    this.#reviewTimer = null;
    this.#reviewAbort?.abort();
    this.#reviewAbort = null;
  }
  #syncReviewPolling() {
    if (this.#disposed || !this.pr || (!this.busy.review && this.reviewProgress?.status !== 'running')) {
      this.#stopReviewPolling();
    } else if (this.#reviewTimer === null && this.#reviewAbort === null) {
      this.#reviewTimer = window.setTimeout(() => void this.refreshReviewProgress(), 750);
    }
  }
  async refreshReviewProgress() {
    const identity = this.#identity();
    if (!identity || this.#disposed) return;
    this.#stopReviewPolling();
    const epoch = this.#reviewEpoch;
    const ctrl = new AbortController();
    this.#reviewAbort = ctrl;
    const previous = this.reviewProgress;
    const request = this.#capture();
    try {
      const result = await api.reviewProgress(identity, ctrl.signal);
      if (epoch !== this.#reviewEpoch || !this.#matches(result)) return;
      this.reviewProgress = result.run;
      this.reviewProgressError = null;
      // A review started in another window can finish while this window polls it.
      if (!this.busy.review && previous?.status === 'running' && result.run?.status === 'complete') {
        const saved = await api.state(ctrl.signal);
        if (epoch === this.#reviewEpoch) this.#apply(saved, request);
      }
    } catch (e) {
      if (epoch === this.#reviewEpoch) this.reviewProgressError = `Cannot read review progress: ${errorMessage(e)}`;
    } finally {
      if (epoch === this.#reviewEpoch) {
        this.#reviewAbort = null;
        this.#syncReviewPolling();
      }
    }
  }

  async loadPublishedThreads(force = false) {
    const identity = this.#identity();
    if (!identity || this.#disposed || (this.#threadsAbort && !force)) return;
    if (!force && this.publishedThreads && this.#matches(this.publishedThreads)) return;
    this.#threadsAbort?.abort();
    const epoch = ++this.#threadsEpoch;
    const ctrl = new AbortController();
    this.#threadsAbort = ctrl;
    this.publishedThreadsLoading = true;
    this.publishedThreadsError = null;
    try {
      const result = await api.reviewThreads(identity, ctrl.signal);
      if (epoch !== this.#threadsEpoch || !this.#matches(result)) return;
      this.publishedThreads = result;
      for (const draft of Object.values(this.replyDrafts)) {
        if (draft.prKey === result.prKey && draft.uncertain) draft.refreshed = true;
      }
    } catch (e) {
      if (epoch === this.#threadsEpoch) this.publishedThreadsError = errorMessage(e);
    } finally {
      if (epoch === this.#threadsEpoch) {
        this.#threadsAbort = null;
        this.publishedThreadsLoading = false;
      }
    }
  }
  /** Thread identity is stable across PR commits; the draft retains its original snapshot separately. */
  replyKey(threadId: string) {
    return `${this.pr ? prKeyOf(this.pr) : ''}/${threadId}`;
  }
  beginReply(threadId: string) {
    const identity = this.#identity();
    const thread = this.publishedThreads?.threads.find((t) => t.id === threadId);
    if (!identity || !thread?.canReply) return;
    const key = this.replyKey(threadId);
    this.replyDrafts[key] ??= { ...identity, text: '', pending: false, uncertain: false, refreshed: false };
  }
  replyStale(threadId: string) {
    const draft = this.replyDrafts[this.replyKey(threadId)];
    return !!draft && !this.#matches(draft);
  }
  async postReply(threadId: string): Promise<boolean> {
    const key = this.replyKey(threadId);
    const draft = this.replyDrafts[key];
    if (!draft || draft.pending || draft.uncertain || !draft.text.trim()) return false;
    if (!this.#matches(draft)) {
      draft.error = 'The review snapshot changed. Copy this reply or cancel it and start again on the refreshed thread.';
      return false;
    }
    const blocked = this.blockReason('reply');
    if (blocked) {
      draft.error = blocked;
      return false;
    }
    draft.pending = true;
    const request = this.#capture();
    draft.error = undefined;
    draft.refreshed = false;
    const result = await this.#run('reply', async () => {
      try {
        return await api.replyToReviewThread({ prKey: draft.prKey, baseSha: draft.baseSha, headSha: draft.headSha, threadId, body: draft.text });
      } catch (e) {
        if (e instanceof ApiError && (e.code === 'reply_delivery_uncertain' || e.status === 0 || e.status >= 500 || e.status === 200)) draft.uncertain = true;
        throw e;
      }
    });
    draft.pending = false;
    if (!result) {
      draft.error = this.errors.reply;
      return false;
    }
    if (this.#current(request) && this.#matches(draft)) {
      const thread = this.publishedThreads?.threads.find((t) => t.id === threadId);
      if (thread && !thread.comments.some((c) => c.id === result.id)) thread.comments.push(result);
      // A discussion refresh started before this write must not erase the posted reply.
      if (this.#threadsAbort) void this.loadPublishedThreads(true);
    }
    if (this.replyDrafts[key] === draft) delete this.replyDrafts[key];
    if (this.#current(request)) this.notice = 'Reply posted to GitHub.';
    return true;
  }

  /** Stop background timers (app unmount). */
  dispose() {
    this.#disposed = true;
    this.#stopReviewPolling();
    this.#threadsEpoch++;
    this.#threadsAbort?.abort();
    this.#stopWorkspacePolling();
    this.#stopAnalysisPolling();
    this.#stopPermissionPolling();
  }
  async review() {
    const request = this.#capture();
    const blocked = this.blockReason('review');
    if (blocked) { this.errors.review = blocked; return; }
    this.reviewProgress = null;
    this.reviewProgressError = null;
    const s = await this.#run('review', async () => {
      const request = api.review();
      void this.refreshReviewProgress();
      try { return await request; }
      finally { await this.refreshReviewProgress(); }
    });
    if (s && this.#apply(s, request)) this.notice = 'Agent review finished. Suggested comments are available in Review comments.';
    // A review that fails after the agent replied still saves its report.
    if (!s && this.#current(request)) {
      const saved = await api.state().catch(() => undefined);
      if (saved) this.#apply(saved, request);
    }
  }

  async patchComment(id: string, patch: { body?: string; status?: 'pending' }) {
    const request = this.#capture();
    const c = await this.#run('comment', () => api.patchComment(id, patch));
    if (!c || !this.#current(request) || !this.state) return false;
    this.state.comments = mergeEntities(this.state.comments, [c]);
    return true;
  }
  async deleteComment(id: string): Promise<boolean> {
    const request = this.#capture();
    const tombstone = await this.#run('comment', () => api.deleteComment(id));
    if (!tombstone || !this.#current(request) || !this.state) return false;
    this.state.comments = mergeEntities(this.state.comments, [tombstone]);
    return true;
  }

  async publish(event: ReviewEvent, body: string, expected?: PR) {
    const blocked = expected && snapshotKey(expected) !== snapshotKey(this.pr)
      ? 'The pull request changed. Reopen Finish Review for the current code before submitting.'
      : this.publicationBlockReason;
    if (blocked) { this.errors.publish = blocked; return false; }
    if (event === 'REQUEST_CHANGES' && !body.trim()) {
      this.errors.publish = 'Add a review summary explaining the changes needed.';
      return false;
    }
    if (event === 'COMMENT' && !body.trim() && !this.pendingComments.length) {
      this.errors.publish = 'Add a pending comment or summary, or choose Approve for a bare approval.';
      return false;
    }
    const identity = this.#identity();
    if (!identity) { this.errors.publish = 'Load a pull request before finishing a review.'; return false; }
    const request = this.#capture();
    const s = await this.#run('publish', () => api.publish(identity, event, body.trim() || undefined));
    if (!s || !this.#apply(s, request, { workspace: true })) return false;
    this.notice = 'Review published to GitHub.';
    void this.loadPublishedThreads(true);
    return true;
  }

  /** Whether the local checkout still needs attention after the review round ended. */
  get cleanupWarning(): string | null {
    const w = this.workspace;
    if (!w || w.status !== 'cleanup_failed') return null;
    return w.error || 'The local checkout could not be removed.';
  }

  /** End the local review round: removes the managed checkout only. Never publishes; comments and history stay. */
  /** `discard` carries the id of the exact workspace the user confirmed; it is the only way discardChanges:true is sent. */
  async removeCheckout(discard?: { workspaceId: string }): Promise<boolean> {
    const pr = this.pr;
    if (!pr) return false;
    const request = this.#capture();
    const s = await this.#run('finish', () =>
      api.removeCheckout({ prKey: prKeyOf(pr), baseSha: pr.baseSha, headSha: pr.headSha, ...(discard ? { discardChanges: true, workspaceId: discard.workspaceId } : {}) }),
    );
    if (!s || !this.#apply(s, request, { workspace: true })) return false;
    this.notice = s.workspace?.status === 'cleanup_failed' ? null : 'Local checkout removed. Saved comments and history are kept.';
    return s.workspace?.status !== 'cleanup_failed';
  }

  async chat(message: string): Promise<boolean> {
    const request = this.#capture();
    const threadId = this.activeChatId;
    const draftKey = this.chatDraftKey;
    const draft = this.chatInput;
    if (this.globalThreads.find((thread) => thread.id === threadId)?.stale) {
      this.errors.chat = 'This earlier-head thread cannot be continued. Start a new chat.';
      return false;
    }
    const t = await this.#run('chat', () => api.chat(message, threadId ?? undefined));
    if (!t || !this.#current(request) || !this.state) return false;
    this.state.chats = mergeEntities(this.state.chats, [t]);
    if (this.activeChatId === threadId) this.activeChatId = t.id;
    if (this.conversationDrafts[draftKey] === draft && draft.trim() === message) delete this.conversationDrafts[draftKey];
    return true;
  }

  async verify(question: string, anchor: Selection, threadId?: string): Promise<boolean> {
    const blocked = threadId ? this.verificationThreadBlockReason(threadId) : this.verificationBlockReason;
    if (blocked) {
      if (threadId) this.verificationThreadErrors[threadId] = blocked;
      else this.errors.verify = blocked;
      return false;
    }
    const identity = this.#identity();
    if (!identity) return false;
    const request = this.#capture();
    const draftKey = threadId ? `${request.snapshot}/verify/${threadId}` : null;
    const draft = draftKey ? this.conversationDrafts[draftKey] : undefined;
    if (threadId) this.verificationThreadErrors[threadId] = undefined;
    const accepted = await this.#run('verify', () =>
      api.verify({ ...identity, path: anchor.path, startLine: anchor.start, endLine: anchor.end, side: anchor.side, question, threadId }),
    );
    if (!accepted) {
      if (threadId) this.verificationThreadErrors[threadId] = this.errors.verify;
      return false;
    }
    this.verificationJobs = mergeVerificationJobs(this.verificationJobs, [accepted.job]);
    if (this.#current(request) && this.#matches(accepted.job) && this.state) {
      this.state.chats = mergeEntities(this.state.chats, [accepted.thread]);
      this.notice = null;
      this.lastAcceptedJobId = accepted.job.id;
      this.#syncWorkspacePolling();
    }
    if (draftKey && this.conversationDrafts[draftKey] === draft && draft?.trim() === question) delete this.conversationDrafts[draftKey];
    return true;
  }

  async importStyle(repo?: { owner: string; repo: string }) {
    const request = this.#capture();
    const r = await this.#run('style', () => api.importStyle(repo));
    if (r && this.#current(request) && this.state) {
      this.#metadataVersion++;
      this.state.style = r.style;
      this.notice = `Imported and saved writing style from ${r.sampleCount} existing review comment${r.sampleCount === 1 ? '' : 's'}. You can edit it below.`;
    }
    return this.#current(request) ? r : undefined;
  }
  async saveSettings(s: Settings, save: SettingsSave = {}) {
    const request = this.#capture();
    const r = await this.#run('settings', () => api.saveSettings(s, save));
    if (r && this.#current(request) && this.state) {
      this.#metadataVersion++;
      this.state.settings = r;
      if (save.style !== undefined) this.state.style = save.style;
      this.notice = 'Settings saved.';
      await this.#reacquireAnalysis();
    }
    return !!r && this.#current(request);
  }

  setCollapseTests(value: boolean) {
    this.collapseTests = value;
    try { window.localStorage.setItem('skim.collapseTests', String(value)); }
    catch { this.notice = 'The test-collapse preference could not be saved in this browser. It still applies for this session.'; }
  }

  /** Unit links navigate to their explanation; they do not open a collapsed test file. */
  selectUnit(id: string, expected: { snapshot: string; generation?: string }) {
    if (!this.pr || expected.snapshot !== snapshotKey(this.pr) || expected.generation !== this.state?.analysis?.generation) return;
    const unit = validatedUnits(this.state?.groups ?? [], this.files)?.find((item) => item.id === id);
    const first = unit?.files[0];
    if (!unit || !first) return;
    this.settingsOpen = false;
    this.view = 'pr';
    this.prTab = 'guide';
    this.mobileView = 'diff';
    this.selectedPath = first.path;
    this.fileNavigation = { path: first.path, unitId: id, ...expected, nonce: ++this.#fileNavigationNonce };
  }

  selectFile(path: string, opts: { line?: { side: Side; line: number }; expand?: boolean } = {}) {
    if (!this.files.some((file) => file.path === path)) return;
    this.settingsOpen = false;
    this.view = 'pr';
    if (this.prTab !== 'guide') this.prTab = 'diff';
    this.mobileView = 'diff';
    this.selectionNote = null;
    this.selectedPath = path;
    this.fileNavigation = { path, expand: opts.expand ?? true, ...(opts.line ? { line: { ...opts.line } } : {}), nonce: ++this.#fileNavigationNonce };
  }

  /** A deliberate selection adjustment retains the question, never a manual comment's anchor. */
  selectQuestion(anchor: Selection, returnFocus: string) {
    if (!this.pr) return false;
    if (this.questionStale && this.questionDraft?.question) {
      this.selectionNote = 'Your question belongs to earlier code. Copy it or cancel the unattached question before selecting a fresh anchor.';
      this.questionFocus++;
      return false;
    }
    this.questionDraft = {
      anchor: { ...anchor }, question: this.questionDraft?.question ?? '', returnFocus,
      prKey: prKeyOf(this.pr), baseSha: this.pr.baseSha, headSha: this.pr.headSha,
    };
    this.selectionNote = null;
    return true;
  }

  /** Keyboard / shift-click alternative; a range never spans different sides or hunks. */
  clickLine(path: string, side: Side, line: number, hunk: number, extend: boolean) {
    const s = this.selection;
    if (extend && s && !this.questionStale && (s.path !== path || s.side !== side || s.hunk !== hunk)) {
      this.selectionNote = 'A range must stay in one file, on one side (old or new), within one hunk.';
      return false;
    }
    const anchor = extend && s && !this.questionStale
      ? { ...s, start: Math.min(s.start, line), end: Math.max(s.end, line) }
      : { path, side, hunk, start: line, end: line };
    return this.selectQuestion(anchor, diffElementId(path, 'select', side, line));
  }
}

export const store = new Store();
export type { ReviewComment };
