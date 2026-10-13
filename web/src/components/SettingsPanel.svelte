<script lang="ts">
  import { tick } from 'svelte';
  import { store } from '../lib/store.svelte';
  import type { Settings, ProviderKind, Provider, ProviderKeyUpdate } from '../lib/api';
  import { Button } from '$lib/components/ui/button/index.js';
  import { Input } from '$lib/components/ui/input/index.js';
  import { Textarea } from '$lib/components/ui/textarea/index.js';
  import { Badge } from '$lib/components/ui/badge/index.js';
  import * as NativeSelect from '$lib/components/ui/native-select/index.js';
  import IconButton from '$lib/components/IconButton.svelte';
  import Plus from '@lucide/svelte/icons/plus';
  import Pencil from '@lucide/svelte/icons/pencil';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import ChevronLeft from '@lucide/svelte/icons/chevron-left';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Save from '@lucide/svelte/icons/save';
  import Download from '@lucide/svelte/icons/download';
  import LoaderCircle from '@lucide/svelte/icons/loader-circle';
  import KeyRound from '@lucide/svelte/icons/key-round';
  import ShieldAlert from '@lucide/svelte/icons/shield-alert';
  let providerViewport: HTMLDivElement | undefined = $state();
  let providerViewportWidth = $state(0);
  let providerTableWidth = $state(0);
  let providerScroll = $state(0);
  let settingsLayout = $state<HTMLDivElement>();
  let activeSection = $state('settings-providers');
  const settingsSections = [
    ['settings-providers', 'AI providers'], ['settings-models', 'Model roles'], ['settings-github', 'GitHub'],
    ['settings-agent', 'Verification agent'], ['settings-style', 'Writing style'],
  ] as const;
  $effect(() => {
    const layout = settingsLayout;
    if (!store.settingsOpen || !layout) return;
    const root = layout.closest('.settings-page');
    if (!root) return;
    const sections = [...layout.querySelectorAll<HTMLElement>('section[id]')];
    const updateSection = () => {
      const atEnd = root.scrollTop + root.clientHeight >= root.scrollHeight - 2;
      const line = root.getBoundingClientRect().top + 72;
      const current = atEnd ? sections.at(-1) : sections.filter((section) => section.getBoundingClientRect().top <= line).at(-1) ?? sections[0];
      if (current) activeSection = current.id;
    };
    updateSection();
    root.addEventListener('scroll', updateSection, { passive: true });
    const observer = new ResizeObserver(updateSection);
    observer.observe(root);
    return () => { root.removeEventListener('scroll', updateSection); observer.disconnect(); };
  });
  $effect(() => {
    const id = activeSection;
    void tick().then(() => {
      const nav = settingsLayout?.querySelector<HTMLElement>('.settings-nav');
      const link = nav?.querySelector<HTMLElement>(`a[href="#${id}"]`);
      if (!nav?.clientWidth || !link || nav.scrollWidth <= nav.clientWidth) return;
      const bounds = nav.getBoundingClientRect(), item = link.getBoundingClientRect();
      if (item.left < bounds.left) nav.scrollLeft += item.left - bounds.left;
      else if (item.right > bounds.right) nav.scrollLeft += item.right - bounds.right;
    });
  });

  type Draft = Settings & { argsText: string };
  const clone = (s: Settings): Draft => ({ ...structuredClone($state.snapshot(s)), argsText: JSON.stringify(s.acp.args ?? []) });
  type ProviderTemplate = { id: string; label: string; kind: ProviderKind; baseUrl: string; model: string };
  // Templates only prefill the Add form; saved providers are independent instances.
  const providerTemplates: ProviderTemplate[] = [
    { id: 'openai', label: 'OpenAI', kind: 'openai', baseUrl: 'https://api.openai.com/v1', model: 'gpt-4.1' },
    { id: 'anthropic', label: 'Anthropic (OpenAI-compatible)', kind: 'openai', baseUrl: 'https://api.anthropic.com/v1', model: 'claude-sonnet-4-6' },
    { id: 'typesafe', label: 'TypeSafe · Jev', kind: 'typesafe', baseUrl: 'https://api.typesafe.ai/v1', model: 'jev-latest' },
    { id: 'custom', label: 'Custom endpoint', kind: 'openai', baseUrl: '', model: '' },
  ];
  // Default model suggestions for instances added this session (client-only, not persisted).
  let modelHints: Record<string, string> = {};
  // Password drafts are deliberately separate from Settings and its serialized snapshots.
  let keyInputs = $state<Record<string, string>>({});
  let removeKeys = $state<Record<string, boolean>>({});
  let providerNotice = $state('');

  type AcpMode = 'claude' | 'pi' | 'custom';
  type Link = { href: string; text: string };
  type AcpPreset = { label: string; command: string; install: string[]; setup: string; model: string; links: Link[] };
  // Commands are the adapters' documented executables; Skim never installs or logs in for the user.
  const presets: Record<Exclude<AcpMode, 'custom'>, AcpPreset> = {
    claude: {
      label: 'Claude Code',
      command: 'claude-agent-acp',
      install: ['npm install -g @agentclientprotocol/claude-agent-acp'],
      setup:
        'The adapter runs the Claude Agent SDK, which needs its own Claude authentication on the machine running Skim. A Claude subscription login and an Anthropic API key are different credentials and are not interchangeable; follow Anthropic’s documentation for the one you use.',
      model: 'Model and provider overrides are configured through the adapter’s own documented settings, not Skim.',
      links: [
        { href: 'https://github.com/agentclientprotocol/claude-agent-acp', text: 'Adapter README' },
        { href: 'https://github.com/agentclientprotocol/claude-agent-acp/blob/main/docs/model-configuration.md', text: 'Model configuration' },
        { href: 'https://platform.claude.com/docs/en/agent-sdk/overview', text: 'Claude Agent SDK' },
      ],
    },
    pi: {
      label: 'Pi',
      command: 'pi-acp',
      install: ['npm install -g @earendil-works/pi-coding-agent', 'npm install -g pi-acp'],
      setup:
        'Requires Node.js 22+ and Pi 0.81.0 or newer on PATH. Run pi-acp --terminal-login once in a terminal to sign in. This is the community pi-acp adapter; Pi’s own “pi --mode rpc” is not ACP and cannot be used directly.',
      model: 'Pi providers and models are configured in Pi itself; the adapter has no model flag for Skim to set.',
      links: [
        { href: 'https://github.com/svkozak/pi-acp', text: 'pi-acp README' },
        { href: 'https://pi.dev', text: 'Pi' },
      ],
    },
  };
  const detectMode = (a: Settings['acp']): AcpMode => {
    const noArgs = !a.args?.length;
    return noArgs && a.command === presets.claude.command ? 'claude' : noArgs && a.command === presets.pi.command ? 'pi' : 'custom';
  };
  let acpMode = $state<AcpMode>('custom');
  let customBackup: { command: string; argsText: string } | null = null;

  function pickAcp(next: AcpMode) {
    if (!draft || next === acpMode) return;
    if (acpMode === 'custom') customBackup = { command: draft.acp.command, argsText: draft.argsText };
    if (next === 'custom') {
      if (customBackup) {
        draft.acp.command = customBackup.command;
        draft.argsText = customBackup.argsText;
      }
    } else {
      draft.acp.command = presets[next].command;
      draft.argsText = '[]';
    }
    argsError = null;
    acpMode = next;
  }

  let draft = $state<Draft | null>(null);
  let argsError = $state<string | null>(null);
  let loadedFrom: string | undefined;
  $effect(() => {
    const s = store.state?.settings;
    const saved = s ? JSON.stringify(s) : undefined;
    if (s && saved !== loadedFrom && !store.busy.settings) {
      loadedFrom = saved;
      draft = clone(s);
      acpMode = detectMode(s.acp);
      customBackup = null;
    }
  });

  const roles = [
    ['chat', 'Chat', 'openai', 'Global PR chat'],
    ['review', 'Review', 'openai', 'Agent review, writing-style import, review units, and the factors behind flagged risk'],
    ['jev', 'Jev', 'typesafe', 'Optional. Per-file risk labels and test-only collapsing; leave unset to turn risk off'],
  ] as const;
  const kindOf = (k?: ProviderKind) => k ?? 'openai';

  const problems = $derived.by(() => {
    const out: string[] = [];
    const s = store.state?.settings;
    if (!s) return out;
    if (!s.githubTokenEnv) out.push('GitHub token environment variable name is not set; private PRs, style import and publishing need it (public PRs can load without).');
    if (!s.providers.length) out.push('No AI providers are configured.');
    for (const [id, label, want] of roles) {
      const r = s.roles[id];
      if (!r.provider || !r.model) { if (id !== 'jev') out.push(`${label} role is not configured (provider and model required).`); }
      else {
        const p = s.providers.find((x) => x.id === r.provider);
        if (!p) out.push(`${label} role uses unknown provider “${r.provider}”.`);
        else if (kindOf(p.kind) !== want) out.push(`${label} role needs a ${want === 'typesafe' ? 'TypeSafe' : 'OpenAI-compatible'} provider.`);
      }
    }
    if (!s.acp.command) out.push('ACP agent command is not set; verification is unavailable.');
        return out;
  });

  function newId(): string {
    if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
    return Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, '0')).join('');
  }
  const clearedRoles = (id: string) => roles.filter(([role]) => draft!.roles[role].provider === id).map(([, label]) => label);

  type Editor = {
    mode: 'add' | 'edit'; id: string; original?: Provider; template: string;
    name: string; kind: ProviderKind; baseUrl: string; apiKeyEnv: string; key: string; removeKey: boolean;
  };
  let editor = $state<Editor | null>(null);
  let editorError = $state('');
  let advancedOpen = $state(false);
  let nameInput = $state<HTMLInputElement | null>(null);
  const focusName = () => void tick().then(() => nameInput?.focus());

  function startAdd() {
    if (!draft || editor) return;
    const t = providerTemplates[0]!;
    editor = { mode: 'add', id: newId(), template: t.id, name: '', kind: t.kind, baseUrl: t.baseUrl, apiKeyEnv: '', key: '', removeKey: false };
    editorError = '';
    advancedOpen = false;
    providerNotice = '';
    focusName();
  }
  function pickTemplate(id: string) {
    const t = providerTemplates.find((x) => x.id === id);
    if (!editor || editor.mode !== 'add' || !t) return;
    editor.template = t.id;
    editor.kind = t.kind;
    editor.baseUrl = t.baseUrl;
  }
  function startEdit(id: string) {
    const p = draft?.providers.find((x) => x.id === id);
    if (!p || editor) return;
    editor = {
      mode: 'edit', id, original: { ...p }, template: '', name: p.name || p.id, kind: kindOf(p.kind), baseUrl: p.baseUrl,
      apiKeyEnv: p.apiKeyEnv, key: '', removeKey: !!removeKeys[id],
    };
    editorError = '';
    advancedOpen = false;
    providerNotice = '';
    focusName();
  }
  function cancelEditor() {
    editor = null;
    editorError = '';
  }
  function editorKey(e: KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      cancelEditor();
    } else if (e.key === 'Enter' && e.target instanceof HTMLInputElement) {
      e.preventDefault();
      applyEditor();
    }
  }
  function applyEditor() {
    if (!draft || !editor) return;
    const ed = editor;
    const name = ed.name.trim();
    const url = ed.baseUrl.trim();
    if (!name) return fail('Enter a name for this provider.', true);
    if (Array.from(name).length > 100) return fail('Use at most 100 characters for the name.', true);
    if (!url) return fail('Base URL is required.');
    try {
      const u = new URL(url);
      if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error();
    } catch {
      return fail('Base URL must be a valid http:// or https:// address.');
    }
    const key = ed.key.trim();
    if (ed.mode === 'add') {
      draft.providers.push({ id: ed.id, name, kind: ed.kind, baseUrl: url, apiKeyEnv: ed.apiKeyEnv.trim() });
      const t = providerTemplates.find((x) => x.id === ed.template);
      if (t?.model) {
        modelHints[ed.id] = t.model;
        for (const [role, , kind] of roles) {
          if (kind === ed.kind && !draft.roles[role].provider && !draft.roles[role].model) draft.roles[role] = { provider: ed.id, model: t.model };
        }
      }
      providerNotice = `${name} added (staged). Check the model roles, then press “Save settings”.`;
    } else {
      const p = draft.providers.find((x) => x.id === ed.id);
      if (!p) return fail('This provider no longer exists.');
      p.name = name;
      p.kind = ed.kind;
      p.baseUrl = url;
      p.apiKeyEnv = ed.apiKeyEnv.trim();
      providerNotice = `${name} updated (staged). Press “Save settings” to persist.`;
    }
    if (key) {
      keyInputs[ed.id] = key;
      removeKeys[ed.id] = false;
    } else if (ed.removeKey) {
      delete keyInputs[ed.id];
      removeKeys[ed.id] = true;
    } else if (ed.mode === 'edit') {
      removeKeys[ed.id] = false;
    }
    editor = null;
    editorError = '';
    function fail(msg: string, nameField = false) {
      editorError = msg;
      if (nameField) nameInput?.focus();
      else advancedOpen = true;
    }
  }
  function removeProvider(id: string) {
    if (!draft) return;
    const cleared = clearedRoles(id);
    const label = draft.providers.find((p) => p.id === id)?.name || id;
    draft.providers = draft.providers.filter((p) => p.id !== id);
    delete keyInputs[id];
    delete removeKeys[id];
    delete modelHints[id];
    for (const [role] of roles) {
      if (draft.roles[role].provider === id) draft.roles[role] = { provider: '', model: '' };
    }
    providerNotice = `${label} removal is staged until you save settings; its saved key is deleted then.${cleared.length ? ` Warning: the ${cleared.join(', ')} role${cleared.length > 1 ? 's were' : ' was'} using it and ${cleared.length > 1 ? 'were' : 'was'} cleared — choose a new provider and model before saving. Other roles are unchanged.` : ''}`;
  }
  function selectRoleProvider(role: keyof Settings['roles'], provider: string) {
    if (!draft) return;
    const prev = draft.roles[role].model;
    draft.roles[role] = { provider, model: prev || modelHints[provider] || '' };
  }
  const savedProvider = (id: string) => store.state?.settings.providers.find((x) => x.id === id);
  function effectiveStatus(p: Provider): 'saved' | 'environment' | 'missing' | 'staged' | 'removing' {
    if (keyInputs[p.id]?.trim()) return 'staged';
    if (removeKeys[p.id]) return 'removing';
    return savedProvider(p.id)?.credentialStatus ?? 'missing';
  }
  function pendingOf(p: Provider): string {
    const s = savedProvider(p.id);
    if (!s) return 'Not saved';
    if (s.name !== p.name || s.baseUrl !== p.baseUrl || kindOf(s.kind) !== kindOf(p.kind) || s.apiKeyEnv !== p.apiKeyEnv || keyInputs[p.id]?.trim() || removeKeys[p.id]) return 'Unsaved changes';
    return '';
  }
  async function save(e: SubmitEvent) {
    e.preventDefault();
    if (!draft) return;
    let args: unknown;
    try {
      args = JSON.parse(draft.argsText.trim() || '[]');
    } catch {
      argsError = 'Arguments must be a JSON array of strings, e.g. ["--model","x"].';
      return;
    }
    if (!Array.isArray(args) || args.some((a) => typeof a !== 'string')) {
      argsError = 'Arguments must be a JSON array of strings, e.g. ["--model","x"].';
      return;
    }
    argsError = null;
    const { argsText: _t, ...rest } = $state.snapshot(draft);
    const updates: ProviderKeyUpdate[] = [];
    for (const p of rest.providers) {
      const key = keyInputs[p.id]?.trim();
      if (key) updates.push({ providerId: p.id, apiKey: key });
      else if (removeKeys[p.id]) updates.push({ providerId: p.id, remove: true });
    }
    if (await store.saveSettings({ ...rest, acp: { ...rest.acp, args: args as string[] } }, updates)) {
      for (const update of updates) {
        delete keyInputs[update.providerId];
        delete removeKeys[update.providerId];
      }
      providerNotice = 'Provider settings saved. Keys stay on this machine and are never returned to the browser.';
    }
  }

  let styleText = $state('');
  let styleFrom: string | undefined;
  $effect(() => {
    const s = store.state?.style;
    if (s !== undefined && s !== styleFrom) {
      styleFrom = s;
      styleText = s;
    }
  });
  let importRepo = $state('');
  let importError = $state<string | null>(null);
  async function importStyle() {
    importError = null;
    let repo: { owner: string; repo: string } | undefined;
    if (importRepo.trim()) {
      const m = /^([\w.-]+)\/([\w.-]+)$/.exec(importRepo.trim());
      if (!m) return void (importError = 'Use owner/repo format.');
      repo = { owner: m[1]!, repo: m[2]! };
    }
    await store.importStyle(repo);
  }
</script>

{#if !draft}
  <p class="muted">Settings unavailable.</p>
{:else}
  {#if problems.length}
    <details class="warn">
      <summary class="font-medium">Setup needed <span class="ml-1 tabular-nums">({problems.length})</span></summary>
      <ul class="mt-2">{#each problems as p}<li>{p}</li>{/each}</ul>
    </details>
  {/if}

  <div class="settings-layout" bind:this={settingsLayout}>
  <nav class="settings-nav" aria-label="Settings sections">
    <span class="nav-label">Workspace settings</span>
    {#each settingsSections as [id, label]}
      <a href={`#${id}`} aria-current={activeSection === id ? 'location' : undefined} onclick={(event) => {
        event.preventDefault();
        activeSection = id;
        settingsLayout?.querySelector<HTMLElement>(`#${id}`)?.scrollIntoView({ block: 'start' });
      }}>{label}</a>
    {/each}
  </nav>
  <div class="settings-sections">
  <form onsubmit={save}>

    <section id="settings-providers" class="providers-section" aria-labelledby="providers-heading">
      <div class="section-head">
        <h2 id="providers-heading">AI providers</h2>
        <Button size="sm" onclick={startAdd} disabled={store.busy.settings || !!editor}><Plus class="size-4" aria-hidden="true" />Add provider</Button>
      </div>
      <details class="credential-storage">
        <summary><KeyRound class="mr-1 inline size-3.5" aria-hidden="true" />API keys: plain-text storage</summary>
        <p>Keys are saved on the Skim server in plain text, protected by file permissions (0600 file, 0700 directory), not encryption or an OS keychain. Protect the file and its backups.</p>
        {#if draft.credentialStoragePath}<code>{draft.credentialStoragePath}</code>{/if}
      </details>
      {#if providerNotice}<p class="hint" role="status">{providerNotice}</p>{/if}

      {#if editor}
        {@const ed = editor}
        {@const savedEd = ed.mode === 'edit' ? store.state?.settings.providers.find((x) => x.id === ed.id) : undefined}
        {@const status = savedEd?.credentialStatus ?? 'missing'}
        {@const urlChanged = !!savedEd && status === 'saved' && (ed.baseUrl.trim() !== savedEd.baseUrl || ed.kind !== kindOf(savedEd.kind))}
        <!-- Not a <form>: this lives inside the settings form, so Enter/Escape are handled on the controls. -->
        <div class="provider-editor" role="group" aria-labelledby="pe-title">
          <h3 id="pe-title">{ed.mode === 'add' ? 'Add provider' : `Edit ${ed.original?.name || ed.original?.id}`}</h3>
          {#if ed.mode === 'add'}
            <div class="field">
              <label for="pe-template">Type</label>
              <NativeSelect.Root class="w-full" id="pe-template" onkeydown={editorKey} value={ed.template} onchange={(e) => pickTemplate(e.currentTarget.value)}>
                {#each providerTemplates as t}<option value={t.id}>{t.label}</option>{/each}
              </NativeSelect.Root>
            </div>
          {/if}
          <div class="field">
            <label for="pe-name">Name</label>
            <Input id="pe-name" class="w-full" onkeydown={editorKey} bind:ref={nameInput} bind:value={ed.name} autocomplete="off" placeholder="e.g. Work OpenAI" aria-required="true" />
          </div>
          <div class="field">
            <label for="pe-key">{status === 'saved' ? 'Replace API key' : 'API key'}</label>
            <Input id="pe-key" class="w-full" onkeydown={editorKey} type="password" bind:value={ed.key} oninput={(e) => { if (e.currentTarget.value.trim()) ed.removeKey = false; }} autocomplete="new-password" spellcheck="false" autocapitalize="none" placeholder={status === 'saved' ? 'Key saved — leave blank to keep' : 'Paste your API key'} aria-describedby="pe-key-h" />
            <p class="hint" id="pe-key-h">{status === 'saved' ? 'Blank keeps the saved key, which overrides the environment key.' : status === 'environment' ? 'Using an environment key. A saved key overrides it.' : 'Saved locally; optional for unauthenticated endpoints.'}{ed.mode === 'edit' && keyInputs[ed.id] ? ' A replacement key is staged; blank keeps it.' : ''}</p>
          </div>
          {#if status === 'saved'}
            <div class="provider-actions">
              <Button size="sm" variant="outline" class="text-destructive hover:text-destructive" onkeydown={editorKey} aria-pressed={ed.removeKey} onclick={() => { ed.removeKey = !ed.removeKey; if (ed.removeKey) ed.key = ''; }}><Trash2 class="size-3.5" aria-hidden="true" />{ed.removeKey ? 'Keep saved key' : 'Remove saved key'}</Button>
            </div>
            {#if ed.removeKey}<p class="hint" role="status">The saved key will be removed when you save settings.{ed.apiKeyEnv ? ` Afterwards the environment variable ${ed.apiKeyEnv} is used if it is set.` : ''}</p>{/if}
          {/if}
          {#if ed.baseUrl.startsWith('https://api.anthropic.com')}
            <details class="text-xs text-muted-foreground">
              <summary>Anthropic compatibility &amp; key requirements</summary>
              <p>Uses the OpenAI compatibility endpoint, not the native Messages API. Use a workspace-scoped key; custom workspace headers are not supported. Structured review and reading-order requests use <code>json_schema</code>; endpoint errors are shown as returned.</p>
            </details>
          {/if}
          <details open={advancedOpen || ed.template === 'custom' || !ed.baseUrl.trim()}>
            <summary>Connection settings</summary>
            {#if ed.mode === 'edit'}<p class="hint">Internal ID: <code>{ed.id}</code>. Renaming keeps its key and model assignments.</p>{/if}
            <div class="field">
              <label for="pe-kind">Protocol</label>
              <NativeSelect.Root class="w-full" id="pe-kind" onkeydown={editorKey} bind:value={ed.kind}>
                <option value="openai">OpenAI-compatible</option>
                <option value="typesafe">TypeSafe AI System One (Jev)</option>
              </NativeSelect.Root>
            </div>
            <div class="field"><label for="pe-url">Base URL (required)</label><Input id="pe-url" class="w-full" onkeydown={editorKey} bind:value={ed.baseUrl} inputmode="url" autocomplete="off" spellcheck="false" placeholder="https://api.example.com/v1" aria-required="true" /></div>
            <div class="field"><label for="pe-env">API key environment variable (optional)</label><Input id="pe-env" class="w-full" onkeydown={editorKey} bind:value={ed.apiKeyEnv} autocomplete="off" placeholder="e.g. OPENAI_API_KEY" /></div>
            <p class="hint">Used only without a saved key, including after key removal. Restart Skim after changing its environment.</p>
          </details>
          {#if urlChanged}<p class="hint" role="status">A key is saved for this provider. Changing its endpoint or protocol requires pasting a replacement key or choosing “Remove saved key”, so the old key cannot be forwarded to a new destination.</p>{/if}
          {#if editorError}<div class="error" role="alert">{editorError}</div>{/if}
          <div class="provider-actions">
            <Button size="sm" onkeydown={editorKey} onclick={applyEditor}>{ed.mode === 'add' ? 'Add to settings' : 'Apply changes'}</Button>
            <Button size="sm" variant="outline" onkeydown={editorKey} onclick={cancelEditor}>Cancel</Button>
          </div>
          <p class="hint">Staged until “Save settings”.</p>
        </div>
      {/if}

      {#if !draft.providers.length}
        <p class="provider-empty">No providers yet.</p>
      {:else}
        <div class="table-wrap" role="region" aria-label="AI providers table" bind:this={providerViewport} bind:clientWidth={providerViewportWidth} onscroll={(e) => (providerScroll = e.currentTarget.scrollLeft)}>
          <table class="providers" bind:clientWidth={providerTableWidth}>
            <thead>
              <tr><th scope="col">Name</th><th scope="col">Type</th><th scope="col">Endpoint</th><th scope="col">Key</th><th scope="col"><span class="sr-only">Actions</span></th></tr>
            </thead>
            <tbody>
              {#each draft.providers as p (p.id)}
                {@const status = effectiveStatus(p)}
                {@const pend = pendingOf(p)}
                <tr>
                  <th scope="row">{p.name || p.id}{#if pend}<span class="pending">{pend}</span>{/if}</th>
                  <td><span class="protocol">{kindOf(p.kind) === 'typesafe' ? 'TypeSafe · Jev' : 'OpenAI-compatible'}</span></td>
                  <td class="endpoint"><code>{p.baseUrl || '—'}</code></td>
                  <td><Badge variant="outline" class={status === 'missing' ? 'text-muted-foreground' : status === 'removing' ? 'border-[var(--warn-fg)] text-[var(--warn-fg)]' : 'border-[var(--ok)] text-[var(--ok)]'}>{status === 'saved' ? 'Saved' : status === 'environment' ? 'Environment' : status === 'staged' ? 'Staged' : status === 'removing' ? 'Removal staged' : 'Missing'}</Badge></td>
                  <td class="row-actions">
                    <div class="flex flex-wrap items-center gap-1">
                      <IconButton size="sm" label={`Edit ${p.name || p.id}`} text="Edit" onclick={() => startEdit(p.id)} disabled={store.busy.settings || !!editor}><Pencil class="size-3.5" aria-hidden="true" /></IconButton>
                      <IconButton size="sm" class="text-destructive hover:text-destructive" label={`Remove ${p.name || p.id}`} text="Remove" onclick={() => removeProvider(p.id)} disabled={store.busy.settings || !!editor}><Trash2 class="size-3.5" aria-hidden="true" /></IconButton>
                    </div>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
        {#if providerTableWidth > providerViewportWidth}
          <div class="mb-3 flex justify-end">
            <div class="flex gap-1" role="group" aria-label="Scroll provider columns">
              <IconButton size="icon-sm" variant="outline" label="Scroll providers left" disabled={providerScroll <= 0} onclick={() => providerViewport?.scrollBy({ left: -240, behavior: 'instant' })}><ChevronLeft class="size-4" aria-hidden="true" /></IconButton>
              <IconButton size="icon-sm" variant="outline" label="Scroll providers right" disabled={Math.ceil(providerScroll + providerViewportWidth) >= providerTableWidth} onclick={() => providerViewport?.scrollBy({ left: 240, behavior: 'instant' })}><ChevronRight class="size-4" aria-hidden="true" /></IconButton>
            </div>
          </div>
        {/if}
      {/if}
      <p class="hint">Saved provider or key changes can resend the loaded PR’s evidence.</p>
    </section>

    <section id="settings-models">
      <h2>Model roles</h2>
      <div class="role-grid">
      {#each roles as [id, label, want]}
        {@const r = draft.roles[id]}
        <fieldset>
          <legend>{label}</legend>
          <div class="field">
            <label for="rp{id}">Provider</label>
            <NativeSelect.Root class="w-full" id="rp{id}" value={r.provider} onchange={(e) => selectRoleProvider(id, e.currentTarget.value)}>
              <option value="">(none)</option>
              {#each draft.providers.filter((p) => p.id && kindOf(p.kind) === want) as p}<option value={p.id}>{p.name || p.id}</option>{/each}
              {#if r.provider && !draft.providers.some((p) => p.id === r.provider && kindOf(p.kind) === want)}
                <option value={r.provider}>{r.provider} (not a valid {want} provider)</option>
              {/if}
            </NativeSelect.Root>
          </div>
          <div class="field"><label for="rm{id}">Model</label><Input id="rm{id}" class="w-full" bind:value={r.model} /></div>
        </fieldset>
      {/each}
      </div>
      <details class="mt-3 text-xs text-muted-foreground">
        <summary>Role guide</summary>
        <dl class="mt-2 grid gap-2">
          {#each roles as [, label, , use]}<div><dt class="font-medium text-foreground">{label}</dt><dd>{use}</dd></div>{/each}
        </dl>
      </details>
    </section>
    <section id="settings-github">
      <h2>GitHub</h2>
      <div class="field">
        <label for="gh">Token environment variable name</label>
        <Input id="gh" class="w-full" bind:value={draft.githubTokenEnv} placeholder="GITHUB_TOKEN" aria-describedby="gh-h" />
        <p class="hint" id="gh-h">Name only. The server reads the token from its environment.</p>
      </div>
    </section>

    <section id="settings-agent">
      <h2>Verification agent (ACP)</h2>
      <div class="warn flex items-start gap-2">
        <ShieldAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <p>The local agent can read files and run tools in a prepared PR checkout. It is <strong>not sandboxed</strong>: use a trusted agent and review permission requests. Your own checkouts are untouched.</p>
      </div>
      <div class="field">
        <label for="apreset">Agent</label>
        <NativeSelect.Root class="w-full" id="apreset" value={acpMode} onchange={(e) => pickAcp(e.currentTarget.value as AcpMode)}>
          <option value="claude">Claude Code</option>
          <option value="pi">Pi</option>
          <option value="custom">Custom</option>
        </NativeSelect.Root>
      </div>
      {#if acpMode !== 'custom'}
        {@const p = presets[acpMode]}
        <details class="preset text-xs" aria-live="polite">
          <summary class="cursor-pointer font-medium">Install &amp; authenticate {p.label}</summary>
          <p class="hint mt-3">Skim does not install or sign in for you. Saves <code>{p.command}</code> with no arguments.</p>
          <ol>{#each p.install as cmd}<li><code>{cmd}</code></li>{/each}</ol>
          <p class="hint">{p.setup}</p>
          <p class="hint">{p.model}</p>
          <p class="hint">{#each p.links as l, i}{#if i}{' · '}{/if}<a href={l.href} target="_blank" rel="noopener noreferrer">{l.text}</a>{/each}</p>
        </details>
      {:else}
        <div class="field"><label for="ac">Command</label><Input id="ac" class="w-full" bind:value={draft.acp.command} placeholder="ACP-capable executable" /></div>
        <div class="field">
          <label for="aa">Arguments (including model flags)</label>
          <Input id="aa" class="w-full font-mono" bind:value={draft.argsText} placeholder={'["--flag","value"]'} aria-describedby="aa-h" />
          <p class="hint" id="aa-h">JSON string array. Set the model through agent arguments or configuration.</p>
          {#if argsError}<div class="error" role="alert">{argsError}</div>{/if}
        </div>
      {/if}
      <div class="field">
        <label for="aw">Managed worktree folder (optional)</label>
        <Input id="aw" class="w-full" bind:value={draft.acp.managedRoot} placeholder="Leave empty for the default" aria-describedby="aw-h" />
        <p class="hint" id="aw-h">Absolute path; blank uses Skim’s private default.</p>
        <details class="text-xs text-muted-foreground">
          <summary>Checkout lifecycle</summary>
          <p class="mt-2">Skim creates per-review checkouts{store.state?.settings.acp.managedRoot ? ` in ${store.state.settings.acp.managedRoot}` : ' in its private default folder'}. They are removed when you finish or publish a review.</p>
        </details>
      </div>
      {#if store.pr && store.workspace?.path}
        <p class="hint" data-testid="settings-checkout">Current checkout for this pull request: <code class="mono">{store.workspace.path}</code> ({store.workspace.status.replace('_', ' ')}). Use Remove checkout in the Review comments tab to remove it without publishing.</p>
      {/if}
    </section>

    <div class="savebar">
      {#if store.errors.settings}<div class="error" role="alert">{store.errors.settings}</div>{/if}
      {#if store.blockReason('settings')}<span class="hint" role="status">{store.blockReason('settings')} Unsaved settings are kept.</span>{/if}
      <Button class="ml-auto" type="submit" disabled={!!store.blockReason('settings')}>
        {#if store.busy.settings}<LoaderCircle class="size-4 animate-spin motion-reduce:animate-none" aria-hidden="true" />Saving…{:else}<Save class="size-4" aria-hidden="true" />Save settings{/if}
      </Button>
    </div>
  </form>
  <section id="settings-style">
    <h2>Writing style</h2>
    <p class="hint">Saved separately.</p>
    <div class="field">
      <label for="imp">Import from repository (optional)</label>
      <Input id="imp" class="w-full" bind:value={importRepo} placeholder={store.pr ? `${store.pr.owner}/${store.pr.repo}` : 'owner/repo'} disabled={store.busy.style} aria-describedby="imp-h" />
      <p class="hint" id="imp-h">owner/repo; blank uses the loaded PR.</p>
    </div>
    <Button variant="outline" aria-label={store.busy.style ? 'Working…' : 'Import from my review comments'} onclick={importStyle} disabled={!!store.blockReason('style') || (!store.pr && !importRepo.trim())}>
      {#if store.busy.style}<LoaderCircle class="size-4 animate-spin motion-reduce:animate-none" aria-hidden="true" />Working…{:else}<Download class="size-4" aria-hidden="true" />Import{/if}
    </Button>
    <details class="mt-3 text-xs text-muted-foreground">
      <summary>Import help</summary>
      <p class="mt-2">The review model learns writing guidance from your GitHub review comments. You can edit the result or write your own.</p>
    </details>
    {#if importError}<div class="error" role="alert">{importError}</div>{/if}
    {#if store.errors.style}<div class="error" role="alert">{store.errors.style}</div>{/if}
    {#if store.blockReason('style')}<p class="hint" role="status">{store.blockReason('style')} Unsaved style text is kept.</p>{/if}
    <form onsubmit={(e) => { e.preventDefault(); void store.saveStyle(styleText); }}>
      <label class="text-sm font-medium" for="style">Writing style</label>
      <Textarea id="style" class="min-h-40 w-full" rows={8} bind:value={styleText} disabled={store.busy.style} placeholder="No writing style yet." />
      <Button class="justify-self-start" type="submit" disabled={!!store.blockReason('style') || styleText === store.state?.style}><Save class="size-4" aria-hidden="true" />Save style</Button>
    </form>
  </section>
  </div>
  </div>
{/if}

<style>
  .settings-layout { display: grid; grid-template-columns: 164px minmax(0, 1fr); gap: 32px; align-items: start; }
  .settings-nav { position: sticky; top: 24px; display: flex; flex-direction: column; gap: 4px; }
  .nav-label { font-size: 11px; font-weight: 600; letter-spacing: .04em; text-transform: uppercase; color: var(--fg-muted); padding: 0 0 8px; }
  .settings-nav a { color: var(--fg-muted); border-radius: var(--radius); padding: 8px 10px; font-size: 13px; }
  .settings-nav a:hover, .settings-nav a:focus-visible { color: var(--fg); background: var(--bg-inset); text-decoration: none; }
  .settings-nav a[aria-current] { color: var(--fg); background: var(--bg-inset); box-shadow: inset 2px 0 var(--accent); }
  .settings-sections { min-width: 0; }
  section { margin-bottom: 20px; padding: 24px; border: 1px solid var(--border); border-radius: var(--radius-lg); background: var(--bg); scroll-margin-top: 24px; box-shadow: var(--shadow); }
  h2 { font-size: 17px; margin: 0 0 8px; letter-spacing: -.2px; }
  .role-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; }
  fieldset { min-width: 0; border: 1px solid var(--border); border-radius: var(--radius); margin: 0; padding: 12px; }
  fieldset .field:last-child { margin-bottom: 0; }
  legend { font-size: 13px; font-weight: 600; padding: 0 4px; }
  .table-wrap { overflow-x: auto; max-width: 100%; border: 1px solid var(--border); border-radius: var(--radius); margin-bottom: 12px; }
  table.providers { width: 100%; border-collapse: collapse; font-size: 13px; min-width: 520px; }
  table.providers th, table.providers td { text-align: left; vertical-align: middle; padding: 10px; border-bottom: 1px solid var(--border); }
  table.providers tbody tr:last-child > * { border-bottom: 0; }
  table.providers thead th { font-size: 11px; color: var(--fg-muted); font-weight: 600; background: var(--bg-subtle); }
  table.providers tbody th { min-width: 112px; max-width: 220px; font-size: 13px; font-weight: 600; overflow-wrap: anywhere; }
  table.providers .endpoint { overflow-wrap: anywhere; min-width: 160px; font-size: 12px; color: var(--fg-muted); }
  table.providers .row-actions { white-space: nowrap; }
  .pending { display: block; margin-top: 4px; color: var(--warn-fg); font-size: 11px; font-weight: 400; }
  .protocol { display: inline-block; min-width: 110px; color: var(--fg-muted); font-size: 12px; }
  .credential-storage { color: var(--fg-muted); border: 1px solid var(--border); border-radius: var(--radius); padding: 10px 12px; margin-bottom: 16px; font-size: 12px; }
  .credential-storage summary { font-weight: 500; }
  .credential-storage p { margin: 10px 0; }
  .section-head { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px; margin-bottom: 8px; }
  .section-head h2 { margin: 0; }
  .provider-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 12px 0; }
  .provider-editor { border: 1px solid var(--accent); border-radius: var(--radius); padding: 20px; margin-bottom: 16px; background: var(--bg-subtle); }
  .provider-editor h3 { font-size: 16px; margin: 0 0 16px; }
  .provider-editor details { border-top: 1px solid var(--border); padding-top: 12px; margin-top: 12px; }
  .provider-editor summary { font-size: 13px; margin-bottom: 12px; }
  .provider-empty { border: 1px dashed var(--border); border-radius: var(--radius); padding: 24px; color: var(--fg-muted); text-align: center; margin-bottom: 12px; }
  .preset { border: 1px solid var(--border); border-radius: var(--radius); padding: 12px 16px; margin-bottom: 16px; background: var(--bg-subtle); }
  .preset ol { margin: 8px 0; padding-left: 20px; }
  code { font-family: var(--mono); font-size: 12px; overflow-wrap: anywhere; }
  ul { margin: 4px 0 0; padding-left: 18px; }
  #settings-style form { display: grid; gap: 12px; margin-top: 20px; }
  .savebar { position: sticky; bottom: 0; z-index: 2; display: flex; flex-wrap: wrap; gap: 12px; align-items: center; justify-content: space-between; background: var(--bg); border: 1px solid var(--border); border-radius: var(--radius); padding: 12px 16px; margin: 0 0 20px; box-shadow: var(--shadow); }
  .savebar > .hint { margin: 0; }
  .savebar :global(.error) { flex-basis: 100%; margin: 0; max-height: 30vh; overflow: auto; }
  @media (max-width: 1000px) {
    .settings-layout { grid-template-columns: 1fr; gap: 16px; }
    .settings-nav { top: 0; z-index: 3; flex-direction: row; overflow-x: auto; padding: 8px 0; background: var(--bg-subtle); border-bottom: 1px solid var(--border); }
    .settings-nav a { white-space: nowrap; }
    .settings-nav a[aria-current] { box-shadow: inset 0 -2px var(--accent); }
    .nav-label { display: none; }
    section { scroll-margin-top: 68px; }
  }
  @media (max-width: 600px) {
    section { padding: 16px; }
    .role-grid { grid-template-columns: 1fr; }
    .provider-editor { padding: 14px; }
    .savebar { padding: 12px; }
  }
</style>
