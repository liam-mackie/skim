import type { AppState, FileReview, PR, VerificationJob, WorkspaceStatus } from './api';

export type Busy = 'load' | 'review' | 'verify' | 'chat' | 'publish' | 'style' | 'settings' | 'createComment' | 'comment' | 'finish' | 'reply' | 'refresh';
export const snapshotKey = (pr: PR | null | undefined) => pr ? `${pr.provider}/${pr.owner}/${pr.repo}/${pr.number}@${pr.baseSha}..${pr.headSha}` : '';

const labels: Record<Busy, string> = {
  load: 'A pull request is loading.', review: 'An agent review is running.',
  verify: 'A verification question is being sent.', chat: 'The chat model is responding.',
  publish: 'Your review is being published.', style: 'Writing style is being saved or imported.',
  settings: 'Settings are being saved.', createComment: 'A comment is being saved.', comment: 'A comment is being saved.',
  finish: 'The review checkout is being cleaned up.', reply: 'A reply is being posted to GitHub.',
  refresh: 'Analysis is being started or refreshed.',
};
const contextActions: Busy[] = ['load', 'settings', 'style'];
const aiActions: Busy[] = ['review', 'verify', 'chat'];
const commentActions: Busy[] = ['createComment', 'comment'];

export function conflictReason(action: Busy, context: {
  busy: Partial<Record<Busy, boolean>>;
  reviewRunning: boolean;
  reviewUnavailable: boolean;
  verificationRunning?: number;
  verificationPermissionPending?: boolean;
  workspaceStatus?: WorkspaceStatus;
}): string | null {
  const active = (key: Busy) => !!context.busy[key] || (key === 'review' && context.reviewRunning);
  const reason = (key: Busy) => key === 'review' && context.reviewUnavailable
    ? 'Review progress is unavailable, so Skim cannot confirm the review has finished. Check progress before this action.'
    : labels[key];
  for (const key of contextActions) if (active(key)) return reason(key);
  if (active(action)) return reason(action);
  if (contextActions.includes(action)) {
    for (const key of [...aiActions, 'publish', 'finish', 'reply', ...commentActions, 'refresh'] as Busy[]) {
      if (active(key)) return reason(key);
    }
  }
  if (action === 'publish' || action === 'finish') {
    for (const key of ['review', 'verify', 'publish', 'finish'] as Busy[]) if (active(key)) return reason(key);
    if (context.verificationRunning) return context.verificationPermissionPending
      ? 'A check for this pull request needs permission. Open Activity to respond or cancel before publishing or removing checkouts.'
      : `${context.verificationRunning} check(s) are active for this pull request. Wait or cancel them in Activity before publishing or removing checkouts.`;
  }
  if (action === 'publish') {
    for (const key of ['reply', ...commentActions] as Busy[]) if (active(key)) return reason(key);
  }
  if (action === 'review' || action === 'verify') {
    for (const key of ['publish', 'finish'] as Busy[]) if (active(key)) return reason(key);
  }
  if (action === 'reply' || commentActions.includes(action)) {
    if (active('publish')) return reason('publish');
    if (commentActions.includes(action)) for (const key of commentActions) if (active(key)) return reason(key);
  }
  if (action === 'publish' || action === 'finish') {
    if (context.workspaceStatus === 'preparing') return 'The review checkout is being prepared.';
    if (context.workspaceStatus === 'in_use') return 'This checkout is in use by an agent, possibly in another window. Open Activity to wait or cancel its check.';
  }
  if (action === 'verify' || action === 'publish' || action === 'finish') {
    if (context.workspaceStatus === 'cleanup_pending') return 'The review checkout is being cleaned up.';
  }
  return null;
}

export function verificationActive(job: VerificationJob): boolean {
  return ['queued', 'preparing', 'running', 'summarizing', 'cancelling'].includes(job.status);
}

/** Keep the latest turn per thread; metadata clocks also order same-second follow-ups. */
export function mergeVerificationJobs(current: VerificationJob[], incoming: VerificationJob[]): VerificationJob[] {
  const jobs = new Map(current.map((job) => [JSON.stringify([job.prKey, job.threadId]), job]));
  const phase = { queued: 0, preparing: 1, running: 2, summarizing: 3, cancelling: 4, complete: 5, failed: 5, cancelled: 5 };
  for (const job of incoming) {
    const key = JSON.stringify([job.prKey, job.threadId]);
    const old = jobs.get(key);
    if (!old || (job.revision ?? 0) > (old.revision ?? 0) ||
        ((job.revision ?? 0) === (old.revision ?? 0) && job.id === old.id &&
          phase[job.status] >= phase[old.status] && Date.parse(job.updatedAt) >= Date.parse(old.updatedAt))) jobs.set(key, job);
  }
  return [...jobs.values()];
}

/** IDs are append-only within one PR snapshot. Equal revisions retain the local entity. */
export function mergeEntities<T extends { id: string; revision: number }>(current: T[], incoming: T[]): T[] {
  const merged = new Map(current.map((entity) => [entity.id, entity]));
  for (const entity of incoming) {
    const old = merged.get(entity.id);
    if (!old || (entity.revision ?? 0) > (old.revision ?? 0)) merged.set(entity.id, entity);
  }
  return [...merged.values()];
}

/** Progress is a snapshot-scoped clock; equal revisions retain the saved local value. */
export function mergeFileReview(current: FileReview | undefined, incoming: FileReview | undefined): FileReview | undefined {
  return incoming && (!current || incoming.revision > current.revision) ? incoming : current;
}

/** Full-state and entity clocks are independent; partial responses never advance the former. */
export function mergeState(current: AppState | null, incoming: AppState, preserveAnalysis = false, preserveMetadata = false): AppState {
  if (!current || snapshotKey(current.pullRequest) !== snapshotKey(incoming.pullRequest)) return incoming;
  const next = (incoming.revision ?? 0) > (current.revision ?? 0) ? incoming : current;
  const analysis = current.analysis;
  const keepAnalysis = preserveAnalysis || (analysis && next.analysis?.generation === analysis.generation && analysis.revision > next.analysis.revision);
  const assessments = keepAnalysis ? new Map(current.files.map((file) => [file.path, file])) : null;
  return {
    ...next,
    fileReview: mergeFileReview(current.fileReview, incoming.fileReview),
    comments: mergeEntities(current.comments, incoming.comments),
    chats: mergeEntities(current.chats, incoming.chats),
    ...(keepAnalysis ? { analysis, groups: current.groups, files: next.files.map((file) => ({ ...file, risk: assessments!.get(file.path)?.risk, testOnly: assessments!.get(file.path)?.testOnly })) } : {}),
    ...(preserveMetadata ? { settings: current.settings, style: current.style } : {}),
  };
}
