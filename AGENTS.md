# Notes for coding agents

Project rules, architecture and milestones are in
[docs/PLAN.md](docs/PLAN.md) (working rules + milestones),
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (decisions D1–D15; do not change
without discussing) and [docs/STATUS.md](docs/STATUS.md) (what is verified).
Read them before larger changes.

## Environment

- Go 1.26 (from `go.mod`); run `gofmt`, `go vet ./...`, `go test -race ./...`
  before every commit.
- `make build`, `make dist`, `make test`, `make -C os check` are the usual
  commands. Buildroot output lives in `os/_output/` (ignored, large).
- **GitHub is available**: the `gh` CLI is authenticated as `emdioh` with
  `repo` and `workflow` scopes, and the git remote is
  `https://github.com/emdioh/wa-transcriber`. Push branches, create/edit
  pull requests, watch or re-run CI (`gh run view --log-failed`), tag
  releases and inspect release assets directly. The working branch is
  `claude/brave-gates-yijj1l`; `main` is the protected/default branch.
- **No Docker daemon**: `docker` is installed but
  `/var/run/docker.sock` gives permission denied, so image builds and
  container runs must be done by the owner (give exact commands instead of
  claiming a Docker result). Same for anything needing a WhatsApp account,
  a phone, a board, a WiFi dongle or real network interfaces: tests must use
  the existing fakes/interfaces; ask the owner to run field tests.
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
