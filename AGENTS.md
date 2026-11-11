# AGENTS.md

Guidance for AI coding agents (Claude Code, Cursor, Codex, etc.) and for human contributors using them in this repository.

This file is the single source of truth for "how to contribute here". `CLAUDE.md` exists for tooling that looks for that filename and points to this file.

## Repository

- **Repo:** [`verygoodplugins/whatsapp-mcp`](https://github.com/verygoodplugins/whatsapp-mcp)
- **Origin remote:** always `origin` (this fork). PRs, issues, and `gh` commands target this fork, not the upstream `lharries/whatsapp-mcp`.
- **Default branch:** `main`. All PRs target `main`.
- **Releases:** automated via [release-please](https://github.com/googleapis/release-please) — do **not** hand-edit `CHANGELOG.md` or version numbers.

## Architecture (read first)

Two components, one repo:

```
whatsapp-mcp/
├── whatsapp-bridge/        # Go bridge — talks to WhatsApp Web via whatsmeow
│   ├── main.go             # Startup, config, pairing
│   ├── rest.go             # REST API
│   ├── handlers.go         # Message, history sync and call event handling
│   ├── store.go            # messages.db schema, migrations, queries
│   ├── send.go             # Outbound messages
│   ├── media.go            # Media download, Ogg/Opus analysis
│   ├── content.go          # Text, contact, quote, mention extraction
│   ├── ephemeral.go        # Disappearing messages, revokes
│   ├── group_members.go    # Group member list endpoint
│   ├── history_ondemand.go # On-demand history requests for one chat
│   ├── media_retry.go      # Recover expired media by asking the phone to re-upload
│   ├── transcribe_auto.go  # Auto-transcription queue and backfill
│   ├── internal/transcribe # Transcription engines (whisper.cpp, OpenAI-compatible, ElevenLabs)
│   ├── webhook.go          # Outgoing webhook for incoming messages
│   └── store/              # SQLite (whatsapp.db, messages.db) + media — gitignored
├── whatsapp-mcp-server/    # Python MCP server — exposes tools to AI clients
│   ├── main.py             # FastMCP tool definitions
│   ├── whatsapp.py         # DB queries + bridge HTTP client
│   ├── oauth.py            # Single-user OAuth for the http/sse transports
│   └── audio.py            # FFmpeg helpers
└── .github/                # CI, release, security workflows
```

Data flow: AI client → MCP server (Python) → reads SQLite directly **or** calls bridge REST (`http://localhost:8080/api/*` by default; configurable via `WHATSAPP_API_URL` and `WHATSAPP_BRIDGE_PORT`) → bridge (Go) → WhatsApp Web.

Two SQLite databases:

- `whatsapp.db` — owned by whatsmeow (sessions, contacts, LID map). Treat as opaque.
- `messages.db` — owned by the bridge (chats, messages). Schema is ours.

## Scope rules

Before writing code, check [`ROADMAP.md`](./ROADMAP.md). Anything in "out of scope" should be turned into a polite "won't ship" reply, not a PR.

If unsure whether something is in scope, **open an issue first**. Do not open a PR larger than ~300 LOC without prior discussion.

## PR rules

1. **One concern per PR.** A PR titled "feat: X and also fix Y and refactor Z" gets sent back. Split it.
2. **Conventional commits in the title.** `feat:`, `fix:`, `chore:`, `docs:`, `ci:`, `refactor:`, `test:`, `perf:`. Use `!` (`feat!:`) or `BREAKING CHANGE:` in the body for breaking changes.
3. **Reference an issue** for any `feat:` PR (`Closes #N`). Bug fixes don't strictly require an issue but are easier to review with one.
4. **Update docs in the same PR.** README, `CLAUDE.md`/`AGENTS.md`, or inline tool descriptions if you changed user-visible behavior.
5. **Tests.** Add or update tests for any code you touch in `whatsapp-mcp-server/`. The Go bridge has fewer tests today; matching the existing bar is fine, but don't *remove* coverage.
6. **No drive-by formatting.** Don't reformat files you didn't otherwise change. Keep diffs reviewable.
7. **No new top-level dependencies** without justification in the PR description.
8. **Security-sensitive changes** (auth, file paths, network bind, command exec) get extra scrutiny — call them out in the PR body.

## Local commands

```bash
# Go bridge
cd whatsapp-bridge
go run .                    # dev
go build -o whatsapp-bridge && ./whatsapp-bridge   # release-ish
golangci-lint run           # lint
go test ./...               # tests (sparse today)

# Python MCP server
cd whatsapp-mcp-server
uv sync --extra dev
uv run main.py              # dev
uv run pytest -v            # tests
uv run ruff check .         # lint
uv run ruff format .        # format
```

## PR review and CI

For PR creation, hosted review, CI, and merge readiness, use the installed `babysit` skill (`~/.autovault/skills/babysit/SKILL.md`) as the canonical procedure.

## Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `WHATSAPP_DB_PATH` | `../whatsapp-bridge/store/messages.db` | SQLite path used by the MCP server |
| `WHATSMEOW_DB_PATH` | `../whatsapp-bridge/store/whatsapp.db` | whatsmeow SQLite (LID ↔ phone resolution via `whatsmeow_lid_map`) |
| `WHATSAPP_API_URL` | `http://localhost:8080/api` | Bridge REST endpoint |
| `WHATSAPP_BRIDGE_PORT` | `8080` | Port the bridge binds to |
| `WHATSAPP_BRIDGE_TOKEN` | generated next to `WHATSMEOW_DB_PATH` as `.bridge-token` | Bearer token required for bridge REST calls |
| `WHATSAPP_HUB_URL` | `http://localhost:8767/ui/whatsapp` | AutoHub status/pairing page opened by macOS bridge notifications |
| `WHATSAPP_MEDIA_ROOTS` | `~/.local/share/whatsapp-mcp/outbox` | Path-list of directories allowed for outbound media files |
| `WHATSAPP_DEVICE_NAME` | `whatsmeow` (whatsmeow default) | Linked-device label shown in WhatsApp > Linked Devices. Applied at pair time only (`store.DeviceProps.Os`); re-pair to change |
| `WHATSAPP_MCP_TRANSPORT` | `stdio` | MCP transport to serve clients: `stdio`, `http`, or `sse` |
| `WHATSAPP_MCP_HOST` | `127.0.0.1` | Bind address for the `http`/`sse` transports |
| `WHATSAPP_MCP_PORT` | `8000` | Port for the `http`/`sse` transports |
| `WHATSAPP_MCP_SECRET` | unset | Turns on OAuth for the `http`/`sse` transports (min 32 chars). Entered on the consent page; also accepted as a static bearer token. Required to bind a non-loopback host |
| `WHATSAPP_MCP_PUBLIC_URL` | `http://<host>:<port>` | Public HTTPS origin used as OAuth issuer and resource (e.g. the tunnel hostname) |
| `WHATSAPP_MCP_AUTH_DB` | `mcp-auth.db` next to `WHATSAPP_DB_PATH` | SQLite file for OAuth clients, hashed tokens and the tool-call audit log |
| `WEBHOOK_URL` | `http://localhost:8769/whatsapp/webhook` | Outgoing webhook for incoming messages (empty falls back to this default) |
| `WEBHOOK_ENABLED` | `true` | Set to `false` to disable outbound webhooks entirely |
| `WHATSAPP_AUTO_DOWNLOAD_MEDIA` | `true` | Automatically download incoming media, including webhook image bytes. `false` keeps webhook metadata/text without `mediaBase64` and leaves downloads to `/api/download` (`download_media`); delayed downloads may fail after media expires. Status messages are stored but never auto-downloaded or forwarded |
| `FORWARD_SELF` | `true` | Whether self-sent messages are forwarded (`getEnvBool` default; set `FORWARD_SELF=false` to disable) |
| `WHATSAPP_PARENT_WATCHDOG_S` | `30` | Stdio parent-liveness poll interval (seconds). Exits when the original parent is gone (POSIX reparent). Soft stdin EOF alone does not exit. |
| `WHISPER_MODEL` | unset | Required model-file path for default whisper.cpp transcription |
| `WHISPER_LANGUAGE` | `auto` | Language for whisper.cpp transcription |
| `WHATSAPP_TRANSCRIPTION_PROVIDER` | `whisper_cpp` | `whisper_cpp`, `openai_compatible` or `elevenlabs`; used by the tool and by bridge auto-transcription |
| `WHATSAPP_TRANSCRIPTION_URL` | unset | Required full HTTP(S) endpoint for `openai_compatible`; audio is uploaded here, use loopback to keep it local |
| `WHATSAPP_TRANSCRIPTION_MODEL` | unset | Required model ID for the HTTP provider (e.g. `parakeet`) |
| `WHATSAPP_TRANSCRIPTION_LANGUAGE` | `auto` | HTTP provider language; `auto` omits the language field |
| `WHATSAPP_TRANSCRIPTION_API_KEY` | unset | Optional bearer token for the HTTP provider |
| `ELEVENLABS_API_KEY` | unset | Required for the `elevenlabs` provider; audio is uploaded to ElevenLabs. Model defaults to `scribe_v2` |
| `WHATSAPP_AUTO_TRANSCRIBE` | `off` | Bridge transcribes incoming audio after download: `off`, `voice` (voice notes only) or `audio` |
| `WHATSAPP_AUTO_TRANSCRIBE_MAX_SECONDS` | `0` | Skip auto-transcription of recordings longer than this (0 = no cap) |
| `WHATSAPP_AUTO_TRANSCRIBE_CHATS` | unset | Comma-separated chat JIDs; when set, only these chats are auto-transcribed |
| `WHATSAPP_AUTO_TRANSCRIBE_EXCLUDE_CHATS` | unset | Comma-separated chat JIDs never auto-transcribed |

When adding a new env var: document it here, in `README.md`, and in `.env.example`.

## Gotchas (read before editing)

1. **JIDs.** WhatsApp identifies users as `1234567890@s.whatsapp.net` (DM), `123456@g.us` (group), and `<random>@lid` (link-ID, anonymous). The bridge maintains a phone↔LID map in `whatsapp.db.whatsmeow_lid_map`. Many "user is missing" / "messages don't show" bugs trace back to JID-form mismatches. Always think about both forms.
2. **Media files** live under `store/{chat_jid}/` with timestamp + message-ID filenames. Don't hand-construct these paths in client code; use the bridge's `/api/download` endpoint.
3. **Audio.** WhatsApp voice messages must be Opus `.ogg`. The MCP server's `send_audio_message` tool auto-converts via FFmpeg if installed.
4. **History sync** is controlled by the *primary* device (the phone). The bridge can request more at pair time (see the `--full-history-pair` flag) or for a single chat at runtime (`POST /api/history`, see `history_ondemand.go`), but the phone has the final word.
5. **`messages.db` is the source of truth for the MCP server.** Don't make the MCP server dependent on the bridge being up for *read* operations. The server opens it read-only via `_connect_messages_db()` and raises `FileNotFoundError` if it is missing rather than letting `sqlite3.connect` create an empty one; keep new read paths on that helper.
6. **Outgoing calls are not visible to linked devices.** Don't promise features that depend on them.
7. **Intel macOS dependency builds.** Patched `cryptography` is required even when Intel Mac wheels are unavailable. Source builds need Rust, OpenSSL, pkg-config, and Xcode Command Line Tools (see README prerequisites); do not restore the old `<49` pin to avoid installing build tools.

## Where to make changes

| You want to… | Touch this file |
|---|---|
| Add or modify an MCP tool | `whatsapp-mcp-server/main.py` |
| Change DB queries / data conversion | `whatsapp-mcp-server/whatsapp.py` |
| Change bridge REST API | `whatsapp-bridge/rest.go` |
| Change bridge event handling | `whatsapp-bridge/handlers.go` |
| Change `messages.db` schema or queries | `whatsapp-bridge/store.go` |
| Change webhook payload | `whatsapp-bridge/webhook.go` |
| Change CI behavior | `.github/workflows/*.yml` |
| Change release behavior | `release-please-config.json`, `.release-please-manifest.json` |

## Persona for AI agents working in this repo

- **Be terse.** Don't restate the question.
- **Be decisive.** Pick the smallest change that fixes the problem.
- **Bias to action** for low-risk improvements (lint, tests, error messages, comments that explain *why*).
- **Ask** before architectural changes, dependency additions, or anything in `ROADMAP.md`'s "out of scope".
- **Cite files with `path:line`** when discussing code.
- **Never** edit `CHANGELOG.md`, version constants in `pyproject.toml`/`go.mod`, or `.release-please-manifest.json` directly.

## Reporting bugs / requesting features

- Bugs: use the **Bug report** issue template. Include bridge + MCP server versions, OS, exact reproduction.
- Features: use the **Feature request** template. State the problem, not the solution. Confirm it fits `ROADMAP.md`.

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for the human-facing contribution guide.
