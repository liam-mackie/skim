import type { AnalysisTask, Risk } from './api';

export type FlaggedRisk = Risk & { level: 'medium' | 'high' };

/** Mirrors the server's model.RiskFlagThreshold; the server applies it, this only explains it. */
export const RISK_FLAG_THRESHOLD = 0.8;

/** What each displayed level claims. Wording follows the server's Jev criteria. */
export const RISK_MEANING: Record<FlaggedRisk['level'], string> = {
  high: 'If this change is wrong, the diff shows a credible path to unauthorized access, data exposure or loss, a serious outage, or broad compatibility breakage.',
  medium: 'If this change is wrong, the impact is meaningful but bounded: a behavior, integration, compatibility or operational change without a concrete high-impact consequence.',
};

export const RISK_SCOPE = `Risk rates the impact if this change is wrong, not whether it has a bug. Files are flagged only when the risk model is at least ${Math.round(RISK_FLAG_THRESHOLD * 100)}% confident; unflagged files still need review.`;

export const isFlagged = (risk: Risk | undefined): risk is FlaggedRisk => risk?.level === 'medium' || risk?.level === 'high';

export const riskLabel = (risk: FlaggedRisk) => `${risk.level === 'high' ? 'High' : 'Medium'} risk`;

/** One-line explanation for tooltips and accessible labels. */
export function riskSummary(risk: FlaggedRisk): string {
  const first = risk.factors?.[0]?.text;
  return `${riskLabel(risk)} (${Math.round(risk.probability * 100)}% confident). ${first ?? RISK_MEANING[risk.level]}`;
}

/** Factor state for a flagged file; `pending` while the risk task is still explaining. */
export function factorState(risk: FlaggedRisk, task: AnalysisTask | undefined): 'ready' | 'none' | 'pending' | 'error' | 'missing' {
  if (risk.factors?.length) return 'ready';
  if (risk.factors) return 'none';
  if (risk.factorsError) return 'error';
  return task?.status === 'running' ? 'pending' : 'missing';
}
