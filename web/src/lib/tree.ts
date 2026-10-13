import type { DiffFile, RiskLevel } from './api';

/** Paths are untrusted text from the server/GitHub: they are only ever split, compared and rendered as text. */
export type TreeDir = {
  kind: 'dir';
  /** Full path of the deepest directory in this (possibly compacted) chain; unique identifier. */
  path: string;
  /** Display label: one or more single-child directory names joined with "/". */
  label: string;
  children: TreeNode[];
  fileCount: number;
  additions: number;
  deletions: number;
  risk?: RiskLevel;
};
export type TreeFile = { kind: 'file'; path: string; name: string; file: DiffFile };
export type TreeNode = TreeDir | TreeFile;

const rank: Record<RiskLevel, number> = { low: 1, medium: 2, high: 3 };
export const maxRisk = (a: RiskLevel | undefined, b: RiskLevel | undefined) =>
  !a ? b : !b ? a : rank[b] > rank[a] ? b : a;

type Mut = { dirs: Map<string, Mut>; files: TreeFile[] };
const mut = (): Mut => ({ dirs: new Map(), files: [] });

const segments = (path: string) => path.split('/').filter((s) => s !== '');

/** Directories first, then files, each ordered by name (natural-numeric, case-insensitive) like a file browser. */
const byName = (a: string, b: string) => a.localeCompare(b, undefined, { numeric: true, sensitivity: 'base' }) || (a < b ? -1 : a > b ? 1 : 0);

function finish(m: Mut, prefix: string): TreeNode[] {
  const dirs: TreeDir[] = [];
  for (const [name, child] of [...m.dirs].sort(([a], [b]) => byName(a, b))) {
    let label = name;
    let path = prefix ? `${prefix}/${name}` : name;
    let cur = child;
    // Compact chains of directories that contain exactly one directory and nothing else.
    while (cur.files.length === 0 && cur.dirs.size === 1) {
      const [[n, next]] = [...cur.dirs] as [[string, Mut]];
      label += `/${n}`;
      path += `/${n}`;
      cur = next;
    }
    const children = finish(cur, path);
    let fileCount = 0, additions = 0, deletions = 0;
    let risk: RiskLevel | undefined;
    for (const c of children) {
      if (c.kind === 'dir') {
        fileCount += c.fileCount; additions += c.additions; deletions += c.deletions; risk = maxRisk(risk, c.risk);
      } else {
        fileCount++; additions += c.file.additions; deletions += c.file.deletions; risk = maxRisk(risk, c.file.risk?.level);
      }
    }
    dirs.push({ kind: 'dir', path, label, children, fileCount, additions, deletions, risk });
  }
  const files = [...m.files].sort((a, b) => byName(a.name, b.name) || (a.path < b.path ? -1 : 1));
  return [...dirs, ...files];
}

export function buildTree(files: readonly DiffFile[]): TreeNode[] {
  const root = mut();
  for (const file of files) {
    const segs = segments(file.path);
    const name = segs.pop() ?? file.path;
    let cur = root;
    for (const s of segs) {
      let next = cur.dirs.get(s);
      if (!next) cur.dirs.set(s, (next = mut()));
      cur = next;
    }
    cur.files.push({ kind: 'file', path: file.path, name, file });
  }
  return finish(root, '');
}

/** All ancestor directory paths ("a", "a/b") for a file path, matching TreeDir.path of uncompacted prefixes. */
export function ancestorDirs(path: string): string[] {
  const segs = segments(path);
  segs.pop();
  const out: string[] = [];
  for (let i = 1; i <= segs.length; i++) out.push(segs.slice(0, i).join('/'));
  return out;
}

export function matchesFilter(f: DiffFile, q: string): boolean {
  const n = q.trim().toLowerCase();
  if (!n) return true;
  return f.path.toLowerCase().includes(n) || (f.previousPath?.toLowerCase().includes(n) ?? false);
}

export type Unit = { id: string; title: string; summary?: string; files: DiffFile[] };

/** Opaque explanation anchor, shared by unit links and the continuous guide. */
export function unitElementId(id: string): string {
  return `review-unit-${encodeURIComponent(id)}`;
}

/**
 * Recommended units in backend order with files in backend order, never sorted. The backend guarantees every changed
 * path appears exactly once; anything else (unknown, repeated or omitted paths, empty titles) is treated as no valid
 * recommendation (null) rather than patched up with invented units.
 */
export function validatedUnits(groups: readonly { id: string; title: string; summary?: string; paths: readonly string[] }[], files: readonly DiffFile[]): Unit[] | null {
  const byPath = new Map(files.map((f) => [f.path, f]));
  const seen = new Set<string>();
  const seenUnits = new Set<string>();
  const units: Unit[] = [];
  for (const g of groups) {
    if (!g.id || seenUnits.has(g.id) || !g.title.trim() || g.paths.length === 0) return null;
    seenUnits.add(g.id);
    const fs: DiffFile[] = [];
    for (const p of g.paths) {
      const f = byPath.get(p);
      if (!f || seen.has(p)) return null;
      seen.add(p);
      fs.push(f);
    }
    units.push({ id: g.id, title: g.title, ...(g.summary?.trim() ? { summary: g.summary.trim() } : {}), files: fs });
  }
  return units.length > 0 && seen.size === files.length ? units : null;
}
