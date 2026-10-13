# Skim

Local-first pull-request review. Skim loads a GitHub PR, orders its changes into a guided read, flags the risky files, drafts comments you approve or discard, and sends a coding agent to verify the lines you select. Then it publishes your review to GitHub.

It runs on your machine as a local web app or a native desktop window, on macOS, Linux, and Windows.

## Features

- **Review inbox** — open PRs that request your review or are assigned to you.
- **Guide** — your review model splits the PR into units in reading order, each with a short summary.
- **Risk badges** — optional [TypeSafe Jev](https://typesafe.ai) per-file impact ratings with the factors worth checking; test-only files can collapse automatically.
- **Suggested comments** — AI review drafts comments and GitHub code suggestions. Nothing is published until you approve it.
- **Agent verification** — select lines and ask an [ACP](https://agentclientprotocol.com) agent (Claude Code, Pi, …) to check them in an isolated worktree.
- **Chat, threads, progress** — PR-wide chat, existing GitHub discussions and replies, and per-file Reviewed marks.

## Install

**macOS:** download `Skim-VERSION.dmg` from [Releases](https://github.com/liam-mackie/skim/releases) (universal, signed and notarized) and drag Skim into Applications.

**From source** (Go 1.26+, Node.js 22+, Git):

```sh
make build
./bin/skim        # then open http://127.0.0.1:7437
```

## Getting started

1. Export a GitHub token as `GITHUB_TOKEN`. Public PRs load without one; the inbox, private PRs, and publishing need it (fine-grained: contents read, pull requests read/write).
2. In **Settings**, add a provider: any OpenAI-compatible endpoint for the Chat and Review roles, and optionally TypeSafe for Jev. Keys are saved locally and never sent back to the browser.
3. Optionally pick an ACP agent preset. Install and log in to the agent yourself first:

   | Agent | Install |
   | --- | --- |
   | [Claude Code](https://github.com/agentclientprotocol/claude-agent-acp) | `npm install -g @agentclientprotocol/claude-agent-acp` |
   | [Pi](https://github.com/svkozak/pi-acp) | `npm install -g @earendil-works/pi-coding-agent pi-acp` |

4. Paste a PR URL or pick one from the inbox.

The desktop app imports your login shell's environment when launched from Finder or the Dock, so `GITHUB_TOKEN` and your `PATH` work as they do in a terminal.

## Privacy and safety

- **Opening a PR sends its metadata and patches to your configured review and Jev providers.** That can include private code.
- PR content, chats, drafts, and provider keys are stored in **plain text** (mode `0600`) in the data directory: `~/Library/Application Support/skim` on macOS, `~/.config/skim` on Linux.
- **Agents are not sandboxed.** They run as your user with your environment; use an isolated machine for untrusted code.
- The server only binds loopback and rejects cross-origin requests. Don't expose it on a public interface.

## Configuration

Most settings live in the Settings page. Startup defaults:

| Variable | Default | Purpose |
| --- | --- | --- |
| `SKIM_LISTEN` | `127.0.0.1:7437` | Loopback listen address |
| `SKIM_DATA_DIR` | OS config dir + `skim` | State directory |
| `SKIM_GITHUB_TOKEN_ENV` | `GITHUB_TOKEN` | Variable holding the GitHub token |
| `SKIM_ACP_COMMAND` | | ACP agent executable |
| `SKIM_ACP_ARGS` | | ACP arguments as a JSON string array |
| `SKIM_WORKTREE_ROOT` | `<data-dir>/workspaces` | Parent folder for verification worktrees |
| `SKIM_WEB_DIR` | embedded | `skim` only: serve web assets from this directory |
| `SKIM_DEV_ORIGIN` | | Trusted Vite dev origin |

`--listen`, `--data-dir`, `--web-dir`, and `--dev-origin` override the matching variables.

## Building

The desktop app is a [Wails v2](https://wails.io) shell around the same service (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0`).

| Command | Output |
| --- | --- |
| `make desktop` | `Skim.app` or `Skim.exe` for this machine |
| `make package-macos` | universal `bin/Skim-VERSION.dmg` |
| `make package-windows` | Windows exe and NSIS installer (needs `makensis`) |
| `make package-linux` | Linux binary and `.deb`, built in Docker (`LINUX_ARCH=arm64` for ARM) |

`make package-macos` signs with `MACOS_SIGNING_IDENTITY` and notarizes with `NOTARY_PROFILE` or `NOTARY_APPLE_ID` + `NOTARY_PASSWORD` + `NOTARY_TEAM_ID`; without them the DMG is unsigned. To release, bump `info.productVersion` in `cmd/skim-desktop/wails.json` and push a `vX.Y.Z` tag. CI builds, signs, notarizes, and publishes the DMG.

## Development

```sh
go run ./cmd/skim --dev-origin http://localhost:5173   # terminal 1
npm --prefix web install && npm --prefix web run dev   # terminal 2
make check                                             # tests and type checks
```

See [docs/reference.md](docs/reference.md) for detailed behaviour, persistence and publication guarantees, model limitations, and ACP worktree handling, and [docs/api.md](docs/api.md) for the HTTP API.
