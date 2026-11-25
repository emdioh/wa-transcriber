# Notes for coding agents

Project rules, architecture and milestones are in
[docs/PLAN.md](docs/PLAN.md) (working rules + milestones),
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (decisions D1–D15; do not change
without discussing) and [docs/STATUS.md](docs/STATUS.md) (what is verified).
Read them before larger changes.

## Where we stopped (2026-11-25): the board is a Pi 1, and it does not boot yet

- The owner's Raspberry Pi is a **Pi 1 Model B, silkscreen "2011.12"** (BCM2835,
  **ARMv6**), so the published `rpi2.img` can never boot it: ARMv7 kernel,
  `GOARM=7` binary, bcm2709/bcm2710 dtbs. Stock Raspberry Pi OS boots on the
  same card, board, PSU and HDMI.
- **New board target**: `make -C os rpi1` ->
  `os/_output/rpi1/images/sdcard.img`. arm1176jzf_s toolchain, `bcmrpi` kernel
  6.1.61 (`CONFIG_CPU_V6K`), **13 dtbs covering both naming families**
  (`bcm2708-rpi-*` and `bcm2835-rpi-*`; the firmware picks by model and asks for
  the new names since 5.10), `make build-arm6` ->
  `dist/wa-transcriber-linux-armv6`, `BR2_PACKAGE_WA_TRANSCRIBER_ARCH="armv6"`,
  plus the `linux-armv6` asset and the `rpi1.img` key in `tools/wat-manifest`.
  Docs in `os/README.md`, PLAN M6, STATUS M6, `docs/hardware.md`,
  `docs/image-release.md`.
- **What has been proven**: the rpi1 image builds; our *rpi2* image boots
  end to end under `qemu -M raspi2b`. `qemu -M raspi1ap` is unusable (our
  kernel takes an unhandled external abort on an unemulated device at
  `0xdc975020` before it can find root), so the Pi 1 image is testable only on
  the owner's board.
- **How it fails on the board**: first nothing at all, then (once the boot
  partition was reshaped to Pi OS geometry: p1 at 8 MiB, real FAT32 with
  512-byte clusters, 64 MB) a green LED pattern, "3 short flashes then a long
  pause", still **no rainbow** — i.e. the firmware stops before it draws
  anything, let alone loads a kernel.
- **Bisect in flight** (owner has the cards, no serial adapter): our `zImage`
  + our dtbs copied onto the *stock Pi OS card* with `kernel=zImage` appended.
  The owner reports it "booted completely", which still needs confirming with
  `uname -r` (`6.1.61` = ours, `*+rpt-rpi-v6` = stock). If it was ours, the
  kernel is fine and the fault is our card's boot partition *shape*
  (64 MB p1 vs single-partition layout questions: 512 MB and three partitions
  vs two) — next experiment is our firmware + `config.txt` on the stock card,
  then a stock-shaped rpi1 image.
- `uart_2ndstage=1` is now in `board/rpi1/config.txt`, so the firmware's own
  log appears on GPIO14/15 at 115200 with a 3.3 V USB-TTL adapter. That is the
  fastest way out if the card bisect stalls.
- **Nothing published**: `v0.2.0` still carries the *old* `rpi2.img`; the
  rebuilt rpi2 (13 dtbs) and rpi1 images exist only under `os/_output/`. Do not
  add `rpi1.img` to the release until it boots on the board.
- Tracking: `wat-gjc` (Pi 1 target) is open with these notes. The owner also
  filed M12 (`wat-b14`, "the binary as PID 1", D19) in `1955b24`/`2f7bad2`.

## Where we stopped (2026-11-23)

**The board now boots from its eMMC.** Verified over ttyACM0 after the owner
powered on with the SD card removed: `root=/dev/mmcblk1p2`, USB console back,
`http://192.168.15.51/healthz` = 200. Getting there took four fixes, all in
`internal/flash` and all tested: FAT-legal temp names for the staged files
(a colon gave EINVAL), `partx -u` before mounting the freshly written boot
partition, remount-read-only **and** unmount in the cleanup, and — the one that
made the board look dead — rewriting `root=` in the target's boot file, which
the clone copies verbatim from the card (`/dev/mmcblk0p2` there,
`/dev/mmcblk1p2` on the eMMC). Details in `docs/ARCHITECTURE.md` section 11.

**First thing to look at next session.** The `/data` migration looks *partial*
on the eMMC: `/boot/wat-migrate.d/` is still there (with `config.json`,
`session.db`) and `/data` holds `session.db*` but **no `config.json`**, so
S01data copied some files and then did not finish or did not delete the marker
and the staged directory. Nothing is lost (the SD card still has everything),
but read `dmesg | grep S01data` / `/data` on the eMMC, find out why, fix it, and
then the device comes up with its password and stays paired.

Board state to know about:

- the eMMC eMMC runs the **unpaired** device whose `config.json` I wrote during
  one UI test: the web password is `testtest` wherever that file survives;
- the SD card (2 GB) still holds the working, paired-by-nobody card; it takes
  precedence over the eMMC whenever it is inserted, so putting it back is
  always the way to get the old device;
- `/tmp/wat-new` and `/tmp/flash.log` on the board are my test artifacts
  (tmpfs, gone at the next reboot);
- the images under `os/_output/` are from commit `481ff94`-era sources; the
  Go changes after that are **not** in them (`make -C os bbb` rebuilds — `rpi1` and `rpi2` are the Raspberry Pi
  targets, armv6 and armv7 respectively — and
  `dist/` must be rebuilt first because the binary only rebuilds when its Go
  sources change).

**Open work, in order** (beads carry the detail):

1. finish the `/data` migration check/fix (`wat-3h4.4`);
2. prove the A/B switch **on the eMMC** (edit the eMMC's `extlinux.conf` to
   `mmcblk1p3` and reboot, mirroring what was already proven on the SD);
3. M10: a release carrying an `images` block, then the real install-from-the-
   network test, power cuts, and the U-Boot bootcounter for a kernel that never
   boots (`wat-kfd.2`, `.5`, `.6`);
4. M9 leftovers: the field test on the Pi 2 (`wat-t3l.6`). `!update` in the
   chat was dropped by the owner (`wat-t3l.7`): the web UI is the way;
5. the login-page reset (commit `438737c`) reaches the device only with the
   next image.

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
7. **M9** binary updates from GitHub releases (D17): the code is in; what is
   left is the signing keypair (wat-t3l.5), the field test on both boards
   (wat-t3l.6). `!update` in the chat was dropped on purpose (wat-t3l.7).
8. **M10** image updates with two root slots (D18): code and images done;
   needs the owner's reflash, the field test, and the U-Boot bootcounter for a
   kernel that never boots (wat-kfd.6).
9. **M11** install the SD card onto the BeagleBone Black eMMC: code done
   (`-flash`, System card, `wat-migrate`), the eMMC write itself untested.

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

## Subagents: one worktree each, merged and removed when done

Every subagent writes code in its **own git worktree**, never in the primary
checkout, and the task ends with the branch merged into `main` and the worktree
gone. The primary checkout stays the place where docs, the beads database and
the final commits live.

Create it before dispatching, and hand the path to the subagent as its working
directory:

```bash
git worktree add -b <topic> ../wa-transcriber-<topic>     # e.g. -b backup-restore
```

Tell the subagent, in the task text: this path is its only checkout, it commits
on its own branch, it does not switch branches, push, touch `docs/`, `os/` or
the beads database. Say what it must not edit, because a worktree is not an
isolation boundary for the *board* or the Dolt database — only for the files.

When it reports back:

1. `git -C ../wa-transcriber-<topic> status --short` — a clean worktree is a
   precondition for `git worktree remove`; anything uncommitted there is lost
   when the worktree goes, so harvest it first (a killed subagent leaves edits
   and sometimes useful ones).
2. `git log --oneline --all --decorate -5` — check the work really landed on
   the topic branch. A subagent that commits from the wrong working directory
   puts it on `main` directly (that happened once with the backup feature);
   the decoration shows it before any cleanup.
3. Run the suite **in the worktree**: `gofmt -l cmd internal tools`,
   `go vet ./...`, `go test -race ./...`. A green branch is the merge
   condition, not a green primary checkout.
4. Merge on `main`: `git merge --no-ff <topic>` (a merge commit keeps the
   subagent's work identifiable), then
   `git worktree remove ../wa-transcriber-<topic>`, `git branch -d <topic>`,
   `git worktree prune`. Finish with `git worktree list` showing only the
   primary checkout — if it lists more, the cleanup is not done.
5. File and close the beads in the **primary** checkout, together with the
   status/doc updates, and commit them there.

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
