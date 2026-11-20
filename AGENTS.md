# Notes for coding agents

Project rules, architecture and milestones are in
[docs/PLAN.md](docs/PLAN.md) (working rules + milestones),
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (decisions D1–D15; do not change
without discussing) and [docs/STATUS.md](docs/STATUS.md) (what is verified).
Read them before larger changes.

## Current state (2026-11-20)

Everything that needs no hardware, phone or Docker daemon is done and merged
into `main`:

- M1, M3, M3b (observe-only), M4, M5 (code), M6 (build side) done.
- M7 done on amd64: CI (gofmt, vet, `go test -race`, `make dist`), releases
  `v0.1.0`/`v0.1.1` (3 binaries + `SHA256SUMS`), multi-arch image
  `ghcr.io/emdioh/wa-transcriber` (24.5 MB; 8.4–9.1 MB compressed). The
  container flow was verified with Podman end to end: uid 65532, `healthy`,
  first-visit setup, real pairing, an OpenRouter transcription, pairing
  survives a restart, `!reset` re-exec, and a root-owned legacy volume fails
  fast with the `chown` hint. Only **running the image on an arm host** is
  untested.
- M8 code done: `language` en/it (`internal/bot/i18n.go`, completeness
  test), language select on the System page, `public_url` on the Network
  page when the network is managed by the host, startup log of effective
  non-secret settings. Italian replies are not yet read on a phone.

Remaining work (details and exit criteria in [docs/PLAN.md](docs/PLAN.md)):

1. **M2** invisible coordination signals: needs two WhatsApp accounts and
   phones; write `docs/experiments/M2-results.md`, then decide claim/tag.
2. **M5b** network validation: dongle AP-mode gate, then the eight scenarios
   on a Pi 2 and a Cubieboard.
3. **M6 runtime**: boot < 30 s, RAM budget, 50 power cuts, watchdog test on
   both boards → v1.0.
4. **M7 leftovers**: run the image on an arm64/armv7 host; optionally make
   the ghcr package public (needs `admin:packages`, not available to agents).
5. **M3b**: turn claims on by default only after the M2 decision.
6. Nice-to-have: read the Italian replies in a real chat.

A rootless Podman container from the verification run may still be around
(`podman ps -a`, container `wat`, volume `wat-data`); it is disposable. It
was reset, so it sits in setup mode with no config.

## Environment

- Go 1.26 (from `go.mod`); run `gofmt`, `go vet ./...`, `go test -race ./...`
  before every commit.
- `make build`, `make dist`, `make test`, `make -C os check` are the usual
  commands. Buildroot output lives in `os/_output/` (ignored, large).
- **GitHub is available**: the `gh` CLI is authenticated as `emdioh` with
  `repo` and `workflow` scopes, and the git remote is
  `https://github.com/emdioh/wa-transcriber`. Push branches, create/edit
  pull requests, watch or re-run CI (`gh run view --log-failed`), tag
  releases and inspect release assets directly. `main` is the default and
  working branch; `claude/brave-gates-yijj1l` is the historical feature
  branch that was fast-forwarded into `main` on 2026-11-20. Branch
  protection is not enabled yet.
- **Docker: no daemon** (`/var/run/docker.sock` gives permission denied),
  but **rootless Podman works** for building and running:
  `podman build --format docker -t wa-transcriber:test .` (the default OCI
  format drops `HEALTHCHECK`), `podman run -d --name wat -p 8080:8080
  -v wat-data:/data wa-transcriber:test`, `podman logs wat`, and a `busybox`
  helper to inspect volumes. Short image names need
  `~/.config/containers/registries.conf` with
  `unqualified-search-registries = ["docker.io"]`. The image is `scratch`,
  so use a helper container, not `podman exec`, to look inside.
- Anything needing a real WhatsApp account, a phone, a board, a WiFi dongle
  or real network interfaces must be done by the owner: tests use the
  existing fakes/interfaces.
- Never commit `data/`, `dist/`, the built binary or secrets. Never log
  secrets (API keys, passwords, WiFi passphrases, setup AP key).

## Communication

- Give the owner copy-paste commands for anything that needs GitHub Actions
  minutes, Docker, hardware or a phone, and say exactly what result to send
  back.
- Keep commits imperative and ≤ 72 chars in the subject, with a short body
  saying why. One milestone step per commit/PR when possible.
- Update `docs/STATUS.md` when something is verified or a limitation
  changes; update `docs/ARCHITECTURE.md` when a design detail changes.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:46cd31e7 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   bd dolt push
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->

<!-- BEGIN BEADS CODEX SETUP: generated by bd setup codex -->
## Beads Issue Tracker

Use Beads (`bd`) for durable task tracking in repositories that include it. Use the `beads` skill at `.agents/skills/beads/SKILL.md` (project install) or `~/.agents/skills/beads/SKILL.md` (global install) for Beads workflow guidance, then use the `bd` CLI for issue operations.

### Quick Reference

```bash
bd ready                # Find available work
bd show <id>            # View issue details
bd update <id> --claim  # Claim work
bd close <id>           # Complete work
bd prime                # Refresh Beads context
```

### Rules

- Use `bd` for all task tracking; do not create markdown TODO lists.
- Run `bd prime` when Beads context is missing or stale. Codex 0.129.0+ can load Beads context automatically through native hooks; use `/hooks` to inspect or toggle them.
- Keep persistent project memory in Beads via `bd remember`; do not create ad hoc memory files.

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.
<!-- END BEADS CODEX SETUP -->
