import type { PR } from './api';
import type { PrTab, Store } from './store.svelte';

type Route = { url: string; tab: PrTab; file: string | null } | null;

/** URLs identify providers and PRs, never a cached base/head revision. Reload fetches fresh evidence. */
export function readReviewRoute(location: URL): Route {
  if (location.pathname === '/') return null;
  const match = /^\/github\/([A-Za-z0-9-]+)\/([A-Za-z0-9_.-]+)\/pull\/([1-9]\d*)\/(description|commits|diff|guide)\/?$/.exec(location.pathname);
  if (!match || !Number.isSafeInteger(Number(match[3]))) throw new Error('This review URL is not valid. Open a GitHub pull request from the inbox or URL field.');
  return { url: `https://github.com/${match[1]}/${match[2]}/pull/${match[3]}`, tab: match[4] as PrTab, file: location.searchParams.get('file') };
}

export function reviewPath(pr: PR, tab: PrTab, file: string | null): string {
  const path = `/${pr.provider}/${encodeURIComponent(pr.owner)}/${encodeURIComponent(pr.repo)}/pull/${pr.number}/${tab}`;
  return (tab === 'diff' || tab === 'guide') && file ? `${path}?${new URLSearchParams({ file })}` : path;
}

/** One navigation at a time: rapid Back/Forward keeps the latest target, not the latest HTTP completion. */
export class ReviewNavigation {
  ready = $state(false);
  restoring = $state(false);
  error = $state<string | null>(null);
  #queued: string | null = null;
  #reload = false;
  #running: Promise<void> | null = null;
  #disposed = false;
  #currentPath = '';
  readonly #pop = () => { void this.restore(); };

  constructor(readonly store: Store) {}

  start() {
    window.addEventListener('popstate', this.#pop);
    return this.restore();
  }

  restore(force = false) {
    this.#queued = window.location.href;
    this.#reload = force;
    if (!this.#running) {
      this.#running = this.#drain().finally(() => { this.#running = null; });
    }
    return this.#running;
  }

  async #drain() {
    this.restoring = true;
    this.error = null;
    try {
      while (this.#queued && !this.#disposed) {
        const target = this.#queued;
        const reload = this.#reload;
        this.#reload = false;
        this.#queued = null;
        try {
          const route = readReviewRoute(new URL(target));
          let ok = true;
          if (!this.ready || !this.store.state) {
            ok = await this.store.boot(route?.url);
          } else if (route && (reload || this.store.pr?.url !== route.url)) {
            ok = await this.store.loadPullRequest(route.url);
          }
          if (this.#disposed) return;
          this.ready = true;
          // A newer popstate may already be queued while GitHub was answering.
          if (this.#queued) continue;
          if (!ok) throw new Error(this.store.errors.load ?? 'The pull request could not be loaded.');
          if (route) {
            this.store.resumePr(route.tab);
            if (route.tab === 'diff' || route.tab === 'guide') {
              if (route.file && this.store.files.some((file) => file.path === route.file)) {
                this.store.selectFile(route.file, { expand: false });
              } else {
                this.store.selectedPath = null;
                this.store.fileNavigation = null;
              }
            }
          } else {
            this.store.goHome();
          }
          this.error = null;
          const canonical = this.store.view === 'pr' && this.store.pr
            ? reviewPath(this.store.pr, this.store.prTab, this.store.selectedPath) : '/';
          window.history.replaceState(null, '', canonical);
          this.#currentPath = canonical;
        } catch (error) {
          if (this.#queued || this.#disposed) continue;
          this.ready = true;
          this.error = error instanceof Error ? error.message : 'The review could not be opened.';
        }
      }
    } finally {
      this.restoring = false;
    }
  }

  /** Called reactively for in-app navigation; file selection replaces instead of flooding Back history. */
  sync() {
    const path = this.store.view === 'pr' && this.store.pr
      ? reviewPath(this.store.pr, this.store.prTab, this.store.selectedPath) : '/';
    if (!this.ready || this.restoring || this.error || this.#disposed || path === this.#currentPath) return;
    const fileOnly = path.split('?')[0] === this.#currentPath.split('?')[0];
    window.history[fileOnly ? 'replaceState' : 'pushState'](null, '', path);
    this.#currentPath = path;
  }

  home() {
    window.history.pushState(null, '', '/');
    void this.restore();
  }

  dispose() {
    this.#disposed = true;
    this.#queued = null;
    window.removeEventListener('popstate', this.#pop);
  }
}
