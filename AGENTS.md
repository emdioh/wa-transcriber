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
