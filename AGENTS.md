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

## Work tracking: beads (`bd`)

Work items live in the beads database (`.beads/`, Dolt, issue prefix `wat`),
not in markdown TODO lists. `docs/PLAN.md` stays the milestone *design* (gates,
steps, exit criteria); each active milestone is an epic and each actionable
step a child task. History is committed with the repo, so `.beads` changes go
in the same commit as the work they describe.

```bash
bd ready                  # what can be started now (unblocked)
bd show wat-xxxx          # details
bd update wat-xxxx --claim    # take it
bd close wat-xxxx         # done
bd prime                  # full workflow reference
```

Rules:

- File a bead for every follow-up you discover instead of only mentioning it
  in a reply; put the bead id in the commit body and in `docs/STATUS.md` when
  a milestone state changes.
- Close the bead in the same commit that finishes the work, and update
  `docs/STATUS.md`/`docs/ARCHITECTURE.md` as the rules above require — beads
  does not replace those documents.
- Anything needing a phone, a board or a network stack is owned by the owner:
  create the bead, hand over the exact steps, and keep it open until the
  owner reports the result.
