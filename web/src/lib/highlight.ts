import type { HighlighterCore, LanguageRegistration, ThemeRegistration } from 'shiki/core';
import type { DiffRow } from './diff';

/**
 * Basic GitHub-style syntax highlighting for diff rows.
 *
 * Shiki's core, its JavaScript regex engine (no WASM) and each grammar load lazily via dynamic import,
 * so Vite code-splits them into local assets and nothing is fetched from a CDN. Colors are CSS variables
 * (`--syntax-*` in app.css), so tokens follow the app's light/dark palette.
 */

/** One run of row text. `fontStyle` is Shiki's bit set: 1 italic, 2 bold, 4 underline, 8 strikethrough. */
export type Token = { text: string; color?: string; fontStyle?: number };
export type RowTokens = Map<number, Token[]>;
/** Tokenizes one side of a hunk: one token line per `\n`-separated input line, or null when unavailable. */
export type SideTokenizer = (code: string) => Token[][] | null;

/** Any longer line skips its whole hunk: pathological for regex grammars and rarely readable code. */
export const MAX_LINE_LENGTH = 1000;
/** Files with more parsed rows stay plain text. */
export const MAX_ROWS = 5000;
const CACHE_LIMIT = 200;

type LangModule = { default: LanguageRegistration[] };
/** Keys are Shiki grammar names; each loader is a literal import so Vite emits one chunk per grammar. */
const LANGUAGES = {
  c: () => import('shiki/langs/c.mjs'),
  cpp: () => import('shiki/langs/cpp.mjs'),
  csharp: () => import('shiki/langs/csharp.mjs'),
  css: () => import('shiki/langs/css.mjs'),
  docker: () => import('shiki/langs/docker.mjs'),
  go: () => import('shiki/langs/go.mjs'),
  hcl: () => import('shiki/langs/hcl.mjs'),
  html: () => import('shiki/langs/html.mjs'),
  ini: () => import('shiki/langs/ini.mjs'),
  java: () => import('shiki/langs/java.mjs'),
  javascript: () => import('shiki/langs/javascript.mjs'),
  json: () => import('shiki/langs/json.mjs'),
  jsx: () => import('shiki/langs/jsx.mjs'),
  kotlin: () => import('shiki/langs/kotlin.mjs'),
  lua: () => import('shiki/langs/lua.mjs'),
  make: () => import('shiki/langs/make.mjs'),
  markdown: () => import('shiki/langs/markdown.mjs'),
  php: () => import('shiki/langs/php.mjs'),
  proto: () => import('shiki/langs/proto.mjs'),
  python: () => import('shiki/langs/python.mjs'),
  ruby: () => import('shiki/langs/ruby.mjs'),
  rust: () => import('shiki/langs/rust.mjs'),
  scala: () => import('shiki/langs/scala.mjs'),
  scss: () => import('shiki/langs/scss.mjs'),
  shellscript: () => import('shiki/langs/shellscript.mjs'),
  sql: () => import('shiki/langs/sql.mjs'),
  svelte: () => import('shiki/langs/svelte.mjs'),
  swift: () => import('shiki/langs/swift.mjs'),
  terraform: () => import('shiki/langs/terraform.mjs'),
  toml: () => import('shiki/langs/toml.mjs'),
  tsx: () => import('shiki/langs/tsx.mjs'),
  typescript: () => import('shiki/langs/typescript.mjs'),
  vue: () => import('shiki/langs/vue.mjs'),
  xml: () => import('shiki/langs/xml.mjs'),
  yaml: () => import('shiki/langs/yaml.mjs'),
} satisfies Record<string, () => Promise<LangModule>>;

export type LanguageId = keyof typeof LANGUAGES;
export const LANGUAGE_IDS = Object.keys(LANGUAGES) as LanguageId[];

const EXTENSIONS: Record<string, LanguageId> = {
  go: 'go',
  ts: 'typescript', mts: 'typescript', cts: 'typescript', tsx: 'tsx',
  js: 'javascript', mjs: 'javascript', cjs: 'javascript', jsx: 'jsx',
  svelte: 'svelte', vue: 'vue',
  json: 'json', jsonc: 'json', yaml: 'yaml', yml: 'yaml', toml: 'toml',
  md: 'markdown', markdown: 'markdown',
  css: 'css', scss: 'scss', html: 'html', htm: 'html', xml: 'xml', svg: 'xml', ini: 'ini',
  py: 'python', rb: 'ruby', rs: 'rust', java: 'java', kt: 'kotlin', kts: 'kotlin', cs: 'csharp',
  c: 'c', h: 'c', cpp: 'cpp', cc: 'cpp', cxx: 'cpp', hpp: 'cpp', hh: 'cpp', hxx: 'cpp',
  swift: 'swift', sh: 'shellscript', bash: 'shellscript', zsh: 'shellscript', sql: 'sql',
  dockerfile: 'docker', mk: 'make', proto: 'proto', tf: 'terraform', tfvars: 'terraform', hcl: 'hcl',
  php: 'php', lua: 'lua', scala: 'scala',
};

const BASENAMES: Record<string, LanguageId> = {
  dockerfile: 'docker', containerfile: 'docker', makefile: 'make', gnumakefile: 'make',
  '.bashrc': 'shellscript', '.bash_profile': 'shellscript', '.zshrc': 'shellscript', '.profile': 'shellscript',
};

/** Shiki grammar for a repository path, or null when unknown. */
export function languageFor(path: string): LanguageId | null {
  const base = (path.slice(path.lastIndexOf('/') + 1)).toLowerCase();
  const named = BASENAMES[base];
  if (named) return named;
  if (base.startsWith('dockerfile.')) return 'docker';
  const dot = base.lastIndexOf('.');
  return dot > 0 ? EXTENSIONS[base.slice(dot + 1)] ?? null : null;
}

/**
 * Tokenize each hunk's old side (context + del rows) and new side (context + add rows) as one text, so
 * multi-line strings and comments inside a hunk color correctly. Del rows take old-side tokens; add and
 * context rows take new-side tokens. Hunk and note rows are never tokenized and do not shift alignment.
 *
 * Limitation: the full file is unavailable, so grammar state resets at every hunk; a hunk starting inside
 * a block comment or multi-line string colors from a wrong initial state until the construct closes.
 *
 * A row is highlighted only if its tokens concatenate to exactly `row.text`; otherwise it stays plain.
 */
export function mapTokensToRows(rows: readonly DiffRow[], tokenize: SideTokenizer, maxLineLength = MAX_LINE_LENGTH): RowTokens {
  const result: RowTokens = new Map();
  let oldSide: number[] = [];
  let newSide: number[] = [];
  let tooLong = false;

  const assign = (side: number[]) => {
    if (!side.length) return;
    let lines: Token[][] | null;
    try {
      lines = tokenize(side.map((index) => rows[index]!.text).join('\n'));
    } catch {
      return;
    }
    if (!lines || lines.length !== side.length) return;
    side.forEach((index, line) => {
      const row = rows[index]!;
      if (side === oldSide ? row.kind !== 'del' : row.kind === 'del') return;
      const tokens = merge(lines[line]!);
      if (row.text && tokens.map((t) => t.text).join('') === row.text) result.set(index, tokens);
    });
  };
  const flush = () => {
    if (!tooLong) {
      assign(oldSide);
      assign(newSide);
    }
    oldSide = [];
    newSide = [];
    tooLong = false;
  };

  rows.forEach((row, index) => {
    if (row.kind === 'hunk') flush();
    if (row.kind !== 'context' && row.kind !== 'add' && row.kind !== 'del') return;
    if (row.text.length > maxLineLength) tooLong = true;
    if (row.kind !== 'add') oldSide.push(index);
    if (row.kind !== 'del') newSide.push(index);
  });
  flush();
  return result;
}

/** Join adjacent same-style runs and drop empty ones to keep the DOM small. */
function merge(tokens: readonly Token[]): Token[] {
  const merged: Token[] = [];
  for (const token of tokens) {
    if (!token.text) continue;
    const previous = merged[merged.length - 1];
    if (previous && previous.color === token.color && (previous.fontStyle ?? 0) === (token.fontStyle ?? 0)) previous.text += token.text;
    else merged.push({ text: token.text, color: token.color, fontStyle: token.fontStyle && token.fontStyle > 0 ? token.fontStyle : undefined });
  }
  return merged;
}

/** Inline style for a token; values only ever come from the theme below, never from patch text. */
export function tokenStyle(token: Token): string {
  let style = token.color ? `color:${token.color};` : '';
  const font = token.fontStyle ?? 0;
  if (font & 1) style += 'font-style:italic;';
  if (font & 2) style += 'font-weight:600;';
  const decoration = [font & 4 ? 'underline' : '', font & 8 ? 'line-through' : ''].filter(Boolean).join(' ');
  if (decoration) style += `text-decoration:${decoration};`;
  return style;
}

const THEME_NAME = 'skim-github';
const v = (name: string) => `var(--syntax-${name})`;
/** GitHub's light/dark default scopes (github-light/github-dark), foreground and font style only. */
export const GITHUB_THEME: ThemeRegistration = {
  name: THEME_NAME,
  type: 'light',
  colors: { 'editor.foreground': v('fg'), 'editor.background': 'var(--bg)' },
  fg: v('fg'),
  bg: 'var(--bg)',
  tokenColors: [
    { scope: ['comment', 'punctuation.definition.comment', 'string.comment'], settings: { foreground: v('comment') } },
    { scope: ['constant', 'entity.name.constant', 'variable.other.constant', 'variable.other.enummember', 'variable.language'], settings: { foreground: v('constant') } },
    { scope: ['entity', 'entity.name'], settings: { foreground: v('entity') } },
    { scope: 'variable.parameter.function', settings: { foreground: v('fg') } },
    { scope: 'entity.name.tag', settings: { foreground: v('tag') } },
    { scope: ['keyword', 'storage', 'storage.type'], settings: { foreground: v('keyword') } },
    { scope: ['storage.modifier.package', 'storage.modifier.import', 'storage.type.java'], settings: { foreground: v('fg') } },
    { scope: ['string', 'punctuation.definition.string', 'string punctuation.section.embedded source'], settings: { foreground: v('string') } },
    { scope: ['support', 'meta.property-name', 'support.constant', 'support.variable', 'meta.module-reference', 'string variable'], settings: { foreground: v('constant') } },
    { scope: 'variable', settings: { foreground: v('variable') } },
    { scope: 'variable.other', settings: { foreground: v('fg') } },
    { scope: ['invalid.broken', 'invalid.deprecated', 'invalid.illegal', 'invalid.unimplemented'], settings: { foreground: v('invalid'), fontStyle: 'italic' } },
    { scope: 'message.error', settings: { foreground: v('invalid') } },
    { scope: ['source.regexp', 'string.regexp', 'string.regexp.character-class', 'string.regexp source.ruby.embedded', 'string.regexp string.regexp.arbitrary-repitition'], settings: { foreground: v('string') } },
    { scope: 'string.regexp constant.character.escape', settings: { foreground: v('tag'), fontStyle: 'bold' } },
    { scope: 'punctuation.definition.list.begin.markdown', settings: { foreground: v('variable') } },
    { scope: ['markup.heading', 'markup.heading entity.name'], settings: { foreground: v('constant'), fontStyle: 'bold' } },
    { scope: 'markup.quote', settings: { foreground: v('tag') } },
    { scope: 'markup.italic', settings: { foreground: v('fg'), fontStyle: 'italic' } },
    { scope: 'markup.bold', settings: { foreground: v('fg'), fontStyle: 'bold' } },
    { scope: 'markup.underline', settings: { fontStyle: 'underline' } },
    { scope: 'markup.strikethrough', settings: { fontStyle: 'strikethrough' } },
    { scope: 'markup.inline.raw', settings: { foreground: v('constant') } },
    { scope: ['constant.other.reference.link', 'string.other.link'], settings: { foreground: v('string'), fontStyle: 'underline' } },
  ],
};

let highlighter: Promise<HighlighterCore> | null = null;
const loadedLanguages = new Map<LanguageId, Promise<void>>();

function getHighlighter(): Promise<HighlighterCore> {
  if (!highlighter) {
    highlighter = Promise.all([import('shiki/core'), import('shiki/engine/javascript')])
      .then(([{ createHighlighterCore }, { createJavaScriptRegexEngine }]) =>
        createHighlighterCore({ themes: [GITHUB_THEME], langs: [], engine: createJavaScriptRegexEngine({ forgiving: true }) }));
    highlighter.catch(() => { highlighter = null; });
  }
  return highlighter;
}

/** Load Shiki and one grammar on first use, returning a tokenizer for that language. */
export async function loadTokenizer(lang: LanguageId): Promise<SideTokenizer> {
  const core = await getHighlighter();
  let loading = loadedLanguages.get(lang);
  if (!loading) {
    loading = LANGUAGES[lang]().then((mod: LangModule) => core.loadLanguage(...mod.default));
    loadedLanguages.set(lang, loading);
    loading.catch(() => loadedLanguages.delete(lang));
  }
  await loading;
  return (code) => core.codeToTokensBase(code, { lang, theme: THEME_NAME })
    .map((line) => line.map((t) => ({ text: t.content, color: t.color, fontStyle: t.fontStyle })));
}

/** Most-recently-used results by path and patch; null records files that stay plain. */
const cache = new Map<string, RowTokens | null>();
const pending = new Map<string, Promise<RowTokens | null>>();
const cacheKey = (path: string, patch: string) => `${path}\0${patch}`;

/** Synchronously read a finished result, so remounts render highlighted text without a flash. */
export function cachedRowTokens(path: string, patch: string): RowTokens | null | undefined {
  const key = cacheKey(path, patch);
  const value = cache.get(key);
  if (value !== undefined) {
    cache.delete(key);
    cache.set(key, value);
  }
  return value;
}

/** Tokens per row index for a parsed patch, or null when it stays plain text. Never rejects. */
export function highlightRows(path: string, patch: string, rows: readonly DiffRow[]): Promise<RowTokens | null> {
  const cached = cachedRowTokens(path, patch);
  if (cached !== undefined) return Promise.resolve(cached);
  const key = cacheKey(path, patch);
  let job = pending.get(key);
  if (!job) {
    const lang = languageFor(path);
    job = (lang && rows.length && rows.length <= MAX_ROWS ? loadTokenizer(lang).then((tokenize) => mapTokensToRows(rows, tokenize)) : Promise.resolve(null))
      .then((tokens) => tokens?.size ? tokens : null, () => null)
      .then((tokens) => {
        pending.delete(key);
        cache.set(key, tokens);
        if (cache.size > CACHE_LIMIT) cache.delete(cache.keys().next().value!);
        return tokens;
      });
    pending.set(key, job);
  }
  return job;
}
