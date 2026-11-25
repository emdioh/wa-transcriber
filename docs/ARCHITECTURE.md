# wa-transcriber — architecture and decisions

A single static Go binary that links to the owner's WhatsApp account as a
companion device ("linked device"), transcribes voice notes with a cloud
speech-to-text provider, optionally translates them, and posts the result as
a quoted reply from the owner's own account. It is controlled with a few
`!commands` and a small web UI, and is meant to run headless on a minimal
Linux image (Raspberry Pi 2, BeagleBone Black) or in a Docker container.

Companion documents: [STATUS.md](STATUS.md) (what is done and verified),
[PLAN.md](PLAN.md) (remaining milestones, step by step).

---

## 1. Goals and non-goals

Goals:

- transcribe WhatsApp voice notes automatically (per chat or globally) or on
  demand (`!t`), and post the text as a quoted reply;
- optional translation: languages to keep as they are, one target language
  for everything else;
- control through `!commands` that only the account owner can issue;
- web UI: status, WhatsApp pairing, transcription settings, network, logs;
- headless operation: always reachable through LAN + mDNS (`wa.local`), a
  saved WiFi network, or a setup access point;
- minimal footprint: one binary, no Docker/Node/Chromium/Python/ffmpeg on the
  device, fits in 128–256 MB of RAM, low SD-card wear.

Non-goals: a chat client, other messengers, on-device speech recognition in
v1, several WhatsApp accounts per device, Internet exposure of the UI.

## 2. Big decisions (and why)

| # | Decision | Alternatives considered | Why |
|---|---|---|---|
| D1 | **Go + whatsmeow** for WhatsApp | WAHA (Docker, Node, Chromium) as in Megachat; Baileys (Node); Python wrappers | whatsmeow is a pure-Go implementation of the multi-device protocol: one static binary of ~18 MB, ~30–50 MB RAM, cross-compiles to ARMv7. WAHA needs Docker + a headless browser, impossible on a Pi 2 / BeagleBone Black. |
| D2 | **Pure-Go SQLite** (`modernc.org/sqlite`) for the session | `mattn/go-sqlite3` (cgo) | No cgo → `CGO_ENABLED=0` static builds for every architecture without cross toolchains. |
| D3 | **Cloud transcription** (OpenAI, OpenRouter, Gemini) | whisper.cpp on device | A Pi 2 / BeagleBone Black is far too slow for useful local models. Voice notes are Ogg/Opus, which the APIs accept directly: no ffmpeg. |
| D4 | **New repository**, nothing shared with Megachat | Monorepo with the Python client | Megachat is Python and WAHA-based; only behaviour (commands, freshness window, settings shape) was ported, not code. |
| D5 | **Commands = messages sent by the owner** (`IsFromMe`) | A separate admin number, a bot account | The binary is a linked device of the owner's account, so "sent by me" is exactly "the owner". Contacts can never trigger anything. |
| D6 | **Admin commands only in self chats** ("Message yourself", or a group whose only member is the owner) | Any chat | Replies are visible to everyone in the chat; `!status` contains the device IP, `!password` a password. |
| D7 | **Web UI = server-rendered HTML embedded in the binary** | SPA framework, separate static files | Zero build tooling, works on a phone over the setup AP, ~50 lines of JS. |
| D8 | **Logs in RAM only** (ring buffer ≈1% of RAM, 256 KB–4 MB) | Log files on the SD card | SD wear and corruption on power cuts. Trade-off: logs are lost on reboot. |
| D9 | **Config = one JSON file with atomic writes** (temp + fsync + rename + dir fsync) | SQLite table, several files | Tiny, human-readable, survives power cuts. |
| D10 | **Env vars override config, never persisted** (`WAT_*`) | Env only, config only | Docker-friendly bootstrap while the UI stays the normal way; overridden fields are read-only in the UI. |
| D11 | **Process re-executes itself** for `!reset` / unlink | Exit and rely on a supervisor | Works identically on the image, in Docker and on a dev machine. |
| D12 | **Network management inside the binary** (driving `wpa_supplicant`, `hostapd`, `dnsmasq`, `udhcpc`), **mDNS implemented in Go** | NetworkManager, iwd, avahi | NetworkManager is too heavy; iwd less portable; avahi is another daemon. In-binary mDNS also works in Docker with host networking. (Planned, milestone 5.) |
| D13 | **Buildroot** images with read-only root and a `/data` partition | Raspberry Pi OS, Alpine diskless | Smallest image, fastest boot, power-cut safe, one config per board with shared userspace. (Planned, milestone 6.) |
| D14 | **Multi-transcriber coordination through invisible protocol fields** (empty reaction as claim, tag in the reply) | Visible bot messages, profile "About" text, push name | End-to-end encryption means the server cannot strip extra fields; official clients ignore them. Must be verified first (milestone 2). |
| D15 | **Plain HTTP** for the UI | Self-signed TLS | The device has no domain for a real certificate; self-signed TLS trains users to click through warnings. Acceptable on a home LAN / WPA2 setup AP; the UI must not be exposed to the Internet. |
| D16 | **A carrier loss never touches the interface address**, and neither does a DHCP rebind: the rootfs overlay ships its own idempotent udhcpc script, and flushing is an explicit `Ethernet.Flush` teardown | Flush on carrier down (M5 as first designed); keep the stock script and pass `udhcpc -R` | Flushing the address on a carrier blip kills the WhatsApp websocket, in-flight uploads and mDNS sockets — and the boards flap right after boot (wat-f2z.9). The destructiveness is in the *script*, not in a client flag: busybox `udhcpc` runs it with `deconfig` before every attempt, and Buildroot's version does `ifconfig $interface 0.0.0.0` there and `ifconfig $interface $ip …` on `bound` — both drop the address. `-R` is worse, not better: it means "release IP on exit", so it runs `deconfig` again on the way out. Our script no-ops on `deconfig` and uses `ip addr replace` (a netlink no-op for an identical address) on `bound`/`renew`; the overlay is rsynced after packages, so it replaces the stock file. |
| D17 | **Binary updates from GitHub releases, executed from `/data/bin` with three slots (active / previous / pending) and the read-only factory binary as the bootstrap** | Remounting the rootfs rw and overwriting `/usr/bin/wa-transcriber`; full A/B image updates (RAUC, swupdate); a Docker self-update | A running executable cannot be replaced anyway, and the rootfs is read-only by design (D13). Keeping the runtime binary on `/data` makes an update one file write plus one `exec`: no partition, bootloader or image work, and the factory binary on the read-only root can never be damaged by a failed update. Three slots (current, previous, next) is the most a small card needs for rollback; a try counter in `state.json` turns a crash loop into an automatic rollback. Docker is out on purpose: the scratch image has nothing to exec from a volume. Full-image updates wait for a second root slot. |
| D18 | **Image updates write the inactive of two root slots; the boot partition (FAT) holds the slot choice and the boot counter; `/data` is never touched** | RAUC/SWUpdate bundles + U-Boot bootchooser; overwriting the single root in place | Two root partitions are the minimum honest A/B: a slot being written is never the one running, and `/data` (session, settings) does not move, so an image update cannot cost a re-pairing. The choice must sit where the bootloader can read *and write* it, which on these boards is the FAT boot partition, not ext4 `/data`. RAUC would work but brings a daemon, a D-Bus and a bundle format for one device class; the health gate we already have (M9) is the same idea in 20 lines. Cost: one reflash, because the partition table changes. |
| D19 | **The binary is the machine's init** on the boards: the bootloader command line carries `init=/usr/bin/wa-transcriber`, so there is no `/etc/inittab`, no `rcS` and no shell in the boot path. The program mounts `/proc`, `/sys`, `/dev`, the tmpfs list, `/boot` (p1, read-only) and `/data` (p4), loads the watchdog module, reaps the children nobody waits for, and turns a fatal error into a reboot instead of an exit | busybox init + the `S00boot`/`S01data`/`rcS` scripts (what the images ship today); a real unikernel (TamaGo/Unikraft); a hand-written init in C | Most of what a unikernel would buy — one process, a 1–2 s boot, no shell to find in the field, a boot path with no script in it — without giving up the one thing only Linux has on this hardware: `wpa_supplicant`/`hostapd` for the WiFi. The scripts it replaces are not deleted: removing the `init=` token from the boot partition boots the old path, which is the rollback for a board that misbehaves. Costs the console shell (opt-in `console=` still logs), and a panic is now a reboot: `internal/proc` has to own every `Wait` for that to be safe. |


## 3. Process layout

```
wa-transcriber (one process, cmd/wa-transcriber)
├── internal/wa          whatsmeow client: pairing, events → bot.Message, sending
├── internal/bot         freshness/dedup filter, !commands, voice-note dispatch
├── internal/transcribe  provider clients, language detection, translation, queue
├── internal/web         HTTP UI, sessions, pages, QR rendering
├── internal/config      config.json (atomic writes, deep-copy reads)
├── internal/status      runtime state (connected, last message, pairing code)
├── internal/logbuf      RAM ring buffer for logs (tee to stderr)
├── internal/coord       (milestone 3b) multi-transcriber coordination
└── internal/netmgr      (milestone 5) ethernet / WiFi / setup AP / mDNS
```

Dependency direction: `main` wires everything; `bot` depends only on
`config` and `status` and talks to WhatsApp through the `bot.Messenger`
interface and to transcription through `bot.Transcriber`; `transcribe` and
`web` depend on `config`/`status`; `wa` depends on `bot` only for the
`bot.Message`/`bot.Quote` types. Nothing depends on `main`.

### Device identity shown to WhatsApp

The companion registers itself as a plain **WhatsApp Web session in Chrome on
Ubuntu** (`internal/wa` `init`: `store.SetOSInfo("Ubuntu",
store.GetWAVersion())` and `DeviceProps.PlatformType = CHROME`), so the phone's
"Linked devices" list reads `Chrome (Ubuntu)` instead of an unknown desktop
client; the pair-code request takes its type from the same field. History sync
is asked for as little as possible (one day, one message per chat) and
`RequireFullSync` stays off. These props are part of the pairing handshake
only: a device paired before the change keeps the identity it registered with
until it is paired again.

### Main flow: a voice note arrives

```
whatsmeow event (*events.Message)
  → wa.convert()         ignore status broadcasts/newsletters; extract text,
                         voice (PTT or Ogg/Opus audio), quoted message,
                         alternate chat address (phone number ↔ LID)
  → go bot.Handle()      (own goroutine: never block whatsmeow)
      status.MessageSeen
      drop edits, messages older than 10 min or than start − 60 s, duplicates
      voice → effective setting on? → transcribe.Service.Transcribe
                 queue slot (1 running, ≤ 8 waiting) → download (≤ 15 MB)
                 → provider call(s) → language check / translation → text
             → wa.Reply(quote the voice note, "🎤 " + text)
      own text starting with "!" → command handler
```

### Commands

| Command | Where | Effect |
|---|---|---|
| `!t` (as a reply to a voice note) | any chat | transcribe that note |
| `!transcription` | any chat | show global / chat / effective state |
| `!transcription on\|off` | any chat | per-chat override |
| `!transcription global on\|off` | any chat | global default |
| `!status` | self chat only | status, incl. web UI URLs `http://ip:port/` |
| `!password [new]` | self chat only | set the UI password (random if omitted); a typed password's message is deleted for everyone |
| `!reset` then `!reset confirm` (60 s) | self chat only | unlink, erase settings/password (and WiFi networks once milestone 5 lands), restart |
| `!help` | self chat only | list commands |

Unknown `!words` are ignored silently (people type `!` for other reasons).
Bot replies never start with `!`, so the bot cannot trigger itself.

### Per-chat settings and LID

WhatsApp addresses a 1:1 chat either by phone number (`…@s.whatsapp.net`) or
by LID (`…@lid`). An override is written under both forms when the
alternate one is known (`MessageSource.SenderAlt/RecipientAlt`); the web UI
groups keys of the same chat into one row (`wa.CanonicalChat`).

## 4. Transcription

- Providers: OpenAI and OpenRouter through the OpenAI-compatible multipart
  `POST {base}/audio/transcriptions`; Gemini through
  `models/{model}:generateContent` with inline base64 audio.
- Defaults: OpenAI `gpt-4o-mini-transcribe` (+ `gpt-4o-mini` for
  translation); OpenRouter `openai/whisper-large-v3` (+ `openai/gpt-4o-mini`);
  Gemini `gemini-flash-latest` (one call does everything).
- Settings precedence: `WAT_*` env > `config.json` > provider defaults,
  resolved again for every voice note (UI changes apply immediately).
- Translation (`keep_languages`, `translate_to`):
  - OpenAI/OpenRouter: transcription, then one chat call returning
    `{"language","translation"}`. Skipped when the transcription API already
    reported a kept language (`whisper-1` with `verbose_json`).
  - Gemini: one call with a JSON response schema
    `{language, transcript, translation}`.
  - The result is always re-checked against the settings: a translation of a
    kept language is dropped; a failed translation still posts the
    transcript.
  - Reply format: `🎤 [de→it] <translation>` + blank line + `🗣 <original>`.
- Limits: one job at a time, 8 waiting (more → `ErrBusy`, logged), 15 MB of
  audio, default timeout 120 s. The API key is masked in every error.

## 5. Web UI

- Routes: `/` status, `/whatsapp` pairing/unlink, `/transcription`
  settings + per-chat overrides, `/network`, `/logs` (+ `/logs.txt`),
  `/system` (password, factory reset), `/setup`, `/login`, `/logout`,
  `/api/status` (JSON polled by the pairing page), `/whatsapp/qr.svg`.
- Listens on `-listen` / `WAT_LISTEN` (default `:80`, empty disables). A
  failure to bind is logged and does not stop transcription.
- Pairing: QR rendered server-side as SVG (rotates every ~20 s; the page
  polls `/api/status` and reloads on change) or "link with phone number"
  (8-character code, `whatsmeow.PairPhone`). Pairing attempts repeat forever.
- Security model:
  - first visit: every page leads to `/setup` until a password exists;
  - bcrypt hash in `config.json`; sessions are random 256-bit tokens in an
    HttpOnly + SameSite=Strict cookie, 30 days, kept in memory and bound to
    the password hash (any change, including `!password`, logs everybody
    out);
  - failed logins serialised with a 1 s delay;
  - POSTs with a foreign `Origin`/`Referer` are refused (403);
  - **the login page can erase the device, but only while nothing is paired.**
    Losing the password *and* the phone left no way in at all: `!password`
    and `!reset` need the chat, the UI needs the password. `POST /reset`
    (unauthenticated) closes that hole and is deliberately narrow: it is
    offered and accepted only when the device is not linked (`WA.Paired()`,
    read from the session store, so it does not depend on WhatsApp being
    reachable), it requires the typed `RESET` like the authenticated one, it
    is logged at WARN, and it does what `!reset` does. On a *linked* device
    the page does not render it and the route answers "send !password or
    !reset in your own chat", so nobody on the LAN can erase a working
    device; every lockout ends unlinked (an unlink, a reset from the phone or
    a lost phone all leave the device unpaired), which is exactly the state
    that opens the door;
  - CSP `default-src 'self'`, no inline scripts, no framing;
  - the API key is write-only (never rendered back).

## 6. Multi-transcriber coordination

Problem: two people in the same chat both run a transcriber → every voice
note is transcribed twice. Constraint: no visible "bot chatter".

Idea: messages are end-to-end encrypted, so extra protobuf fields reach the
other side untouched, and official clients ignore what they do not render.

- **Tag**: every transcription reply carries
  `ContextInfo.conversionSource = "wat/1"`. Peers recognise it and skip that
  voice note. Legacy Megachat replies (`🎤 Trascrizione:` prefix quoting a
  voice note) are recognised too, so a mixed deployment does not duplicate.
- **Claim**: an *empty reaction* on the voice note with
  `groupingKey = "wat/claim/1"` — "remove my reaction" when there is none,
  so nothing is rendered.

Algorithm (`internal/coord`), per voice note, state keyed by `chat|voiceID`
and kept 10 minutes:

1. `rank(jid) = first 8 bytes of sha256(voiceID + "|" + jid.User)` as uint64;
   lower wins. The phone-number JID is used when known (LIDs differ per
   account).
2. Delay: 0 if the owner sent the note, otherwise `1 s + rank(self) % 3 s`.
3. While waiting, a claim or a tagged transcription for the note → stand
   down.
4. Then claim (claim mode), wait a 1.5 s settle window; a claim from a peer
   with a lower rank in that window → stand down.
5. Transcribe; `Lease.StillMine()` just before posting drops the result if a
   lower-rank peer claimed meanwhile (crossing claims).
6. Standing down arms a watchdog of `45 s + note length`: if no tagged
   transcription appears, this instance takes over (claim, no delay).

`!t` bypasses the coordinator: the owner asked explicitly.

**Modes**: observe-only is the default until the M2 experiment proves claims
invisible on real phones (`-coord-claims` / `WAT_COORD_CLAIMS=1` turns claims
on). Observe-only still skips notes that already carry a tagged
transcription, and accepts a rare duplicate. `!status` shows
`coordination: claim|observe-only, claims N, stood down N, took over N`.## 7. Networking (milestone 5)

`-network none|managed` (env `WAT_NETWORK`). Mode `none` is the default and
reports addresses only (Docker, dev machines). Mode `managed` runs as root and
drives everything itself.

Package layout (`internal/netmgr`):

| File | Role |
|---|---|
| `netmgr.go` | `Manager` interface, `Status`, kinds/states, mode selection |
| `none.go` | mode none: groups `status.Addresses()` by interface |
| `sysfs.go` | interface discovery from `/sys/class/net` (`FS` seam) |
| `run.go` | `Runner`: `Run` and `Start` for external commands |
| `ether.go` | ethernet: carrier, DHCP, link-local fallback |
| `store.go` | `/data/wifi.json` with PBKDF2-SHA1 PSKs, atomic writes |
| `wifi.go` | wpa_supplicant config, supervision, status/scan parsing |
| `ap.go` | hostapd + dnsmasq setup AP, setup key from `wat.txt` |
| `policy.go` | `next(state, inputs)` pure state machine |
| `linux.go` | managed mode: discovery, loop, `Manager` methods |
| `clock.go` | wait for a sane clock before WhatsApp connects |
| `mdns/` | in-binary responder for `wa.local` |

Behaviour:

- **Ethernet**: the first poll after discovery always runs the up path,
  even when `/sys/class/net/<if>/carrier` reads 0, because an interface
  nobody has lifted reports no carrier and no later down→up transition
  would ever be seen (this is why the BeagleBone Black got no DHCP lease).
  Up means `ip link set <if> up` then `udhcpc -n` for 15 s; no lease → an
  IPv4 link-local address that passed 2 ARP probes (`arping -D`), retried
  from a MAC hash on collision; DHCP is retried every 60 s and the
  link-local address is dropped on success. Carrier **down** keeps the
  address and only stops DHCP (wat-f2z.9): flushing it killed every
  established TCP socket on a carrier blip, and the boot log has one right
  after boot. The link is not taken down either (`LinkDownOnCarrierLoss` is
  off by default) so routing keeps working. The next carrier-up runs the same
  DHCP command, but the image ships its own `udhcpc` script through the
  rootfs overlay (`os/board/common/rootfs-overlay/usr/share/udhcpc/
  default.script`, verified to win over the package's copy): it does nothing
  on `deconfig` and uses `ip addr replace` on `bound`, so an unchanged lease
  is a netlink no-op and only a changed one replaces the address. The stock
  script does `ifconfig $interface 0.0.0.0` on `deconfig` and `ifconfig
  $interface $ip` on `bound` — a `deladdr` + `addaddr` that drops established
  connections even for the same address. It does not tag resolv.conf entries
  per interface any more, which is fine while one uplink is active at a time.
  `Ethernet.Flush` is the explicit teardown (leaving managed mode, handing the
  interface to another owner).
- **WiFi**: config in `/run/wat/wpa.conf`, SSIDs written as hex so no
  quoting bug is possible; `wpa_supplicant` is supervised and restarted when
  it dies; connect = poll `wpa_cli status` for `COMPLETED` (45 s) then DHCP.
- **Setup AP**: `WA-Setup-XXXX` (WPA2, key from `setup_ap_key=` in the boot
  partition's `wat.txt`, fallback `wa-setup-<6 mac hex>`), `192.168.4.1/24`,
  dnsmasq answers every name with the device address and hands out DHCP
  option 114. No routing to the LAN.
- **Captive portal**: while the AP runs, the OS connectivity-check paths
  (`/generate_204`, `/hotspot-detect.html`, …) and requests for a foreign
  `Host` answer `302 → http://192.168.4.1/`.
- **Policy** (`next`): states `off → connecting → connected | ap`. With
  `auto`, the AP runs while unpaired or while nothing else works; a failed
  client connect goes to the AP (unpaired / no ethernet) or back to `off`
  with a 5 min retry; an idle AP with saved networks goes back to client mode
  after 5 min when one of them is visible. `always`/`never` override.
- **mDNS**: answers `A` for `wa.local` on every interface with that
  interface's IPv4 (TTL 120, cache-flush), to multicast queries and to the
  unicast queries of RFC 6762 section 6.7; a conflicting responder with a
  *different* address makes it rename to `wa-2.local`, `wa-3.local`. Its own
  announcements come back (multicast loopback) and mDNS reflectors on the LAN
  echo them, so a record carrying one of our own addresses is not a conflict.
- **Clock**: before connecting, wait until `now > buildTime` (embedded with
  `-X main.buildTime`) and, in managed mode, `adjtimex` reports `STA_UNSYNC`
  cleared; give up after 2 minutes with a warning; `!status` and the Status
  page then show "clock not synced".
- **Reset**: `!reset` and the web factory reset call `Manager.Erase()`, which
  deletes `wifi.json`.

## 8. Storage

- `/data` (or `-data` / `WAT_DATA`): `session.db` (+ `-wal`, `-shm`),
  `config.json` (0600), later `wifi.json`.
- Image: read-only root, `/tmp` `/run` `/var/log` on tmpfs; only `/data`
  is written (config changes + whatsmeow's small writes per message for its
  encryption state — unavoidable).

## 9. Packaging

- `make build | build-arm7 | build-arm64 | build-amd64 | dist | test | docker`.
- Docker: static binary + CA certificates + tzdata on `scratch`, multi-arch
  through `--platform=$BUILDPLATFORM` cross-compilation.
- Images: Buildroot, one board config per target, shared userspace (M6).

## 10. Memory budget

`GOMEMLIMIT` defaults to a quarter of RAM clamped to 32–256 MB (soft limit).
History sync is reduced to the minimum at pairing and its blobs are never
downloaded (`ManualHistorySyncDownload`), which keeps pairing light on
128 MB boards.

## 11. Binary updates (milestone 9, boards only)

Scope: the *binary*, not the OS image. Docker is explicitly out — the image
is `scratch` and `/wa-transcriber` is immutable, so there the update directory
is unset (the feature is off) and the UI says "pull the image". Full-image
updates need a second root slot plus `swupdate`/RAUC: later, and they force a
card reflash because the partition table changes.

```text
/usr/bin/wa-transcriber          factory binary, read-only rootfs, the bootstrap
/data/bin/state.json             slots + try counter + last failure
/data/bin/wa-transcriber-<ver>   at most three files: active, previous, pending
/data/updates/*.part             transient download, deleted at boot
/data/rollback/<ver>/            config.json + session.db snapshot before a first start
```

`inittab` starts the *factory* binary; before anything else `main` runs the
bootstrap (`-update-dir`, empty = feature off; `-version` and `-healthcheck`
skip it):

1. `pending` present and its hash matches → promote (`previous = active`,
   `active = pending`, delete the file of the slot that fell out of the
   window), `tries = 0`, `verified = false`, then exec it.
2. else `active` → if its hash fails or `tries > 3`, roll back to `previous`
   (and restore the `/data/rollback` snapshot), record `last_failure`, exec
   that; otherwise `tries++` and exec it. A verified `active` is never
   counted.
3. else run the factory binary (fresh card, wiped `/data`, bottom of a
   rollback).

A newer factory binary (card reflashed, `/data` kept) wins over an older
active one, so a flash is always an upgrade; the reverse is never a silent
downgrade. Restart is `syscall.Exec(<bootstrap path>)`, the same mechanism as
`!reset` (D11) — not `os.Executable()`, which would be the `/data` file and
skip the gate, and not `exit`+`respawn`, which busybox throttles (wat-f2z.16).
Exec of a non-executable or wrong-arch file fails *in the old process*, so
that case falls back without a single crash.

Health: the running process sets `verified` and resets `tries` after 60 s of
uptime, not at start, so a process that dies immediately still trips the gate.
A version that starts but misbehaves (WhatsApp refuses the migrated
`session.db`) is reverted by hand from the UI.

`session.db` moves with the binary: whatsmeow migrates it on first start and
an older version may refuse the newer schema, so the outgoing binary copies
`config.json` + `session.db` to `/data/rollback/<version>/` before the first
exec of a new one, and a rollback restores it (losing what arrived in
between).

Trust chain: `manifest.json` from
`github.com/emdioh/wa-transcriber/releases/latest/download/` (a CDN GET, no
API rate limit, no token) plus `manifest.json.ed25519`, a detached ed25519
signature whose public key is embedded with `-X main.updatePubKey` and kept
in `release/update-pubkey.txt`; the private key never leaves the owner's
machine (`tools/wat-sign`). Then: sha256 of the binary, `EM_*` match for the
running arch, size cap, free space for one more binary. An unsigned or
unverifiable manifest is refused. Files are written `.part` → fsync →
rename → fsync dir → `state.json` (the `config.json` scheme, D9), so no state
can ever point at a partial file.

Retention: exactly three slots and at most three files; unreferenced
binaries and `.part` leftovers are pruned at boot.

Checks: manual (System page button, 1 h cache in memory and on disk) plus an
opt-in daily check (`update_check`), never in setup/AP mode; `update_auto`
installs and restarts without asking, still behind the health gate and only
when nothing is queued or transcribing. With both halves present the timer
looks at the **image first**: a release image carries its own binary, so
installing it is one download and one reboot instead of a binary update that
the image would replace a minute later. An image the card cannot take (old
layout, or a manifest needing a newer one) is reported once and the binary
path runs instead.

**The OS image is a separate mechanism (M10, D18)**: two root slots, the
boot partition chooses, `/data` is untouched, and a boot counter falls back
the same way M9's try counter does. The card layout is p1 boot (FAT), p2 root
A, p3 root B, p4 data; both roots are ext4 of the same size (160M, labels
`wat-root-a`/`wat-root-b`), root B is empty in a fresh image, and data is
still made on the first boot. The boot file names the slot by device path
(`root=/dev/mmcblk0p2` for the first boot), computed from the running root so
it works on SD and eMMC alike. The two share the manifest, the signature
and the health timer, nothing else: the binary update is a file on `/data`,
the image update is a block device. M11 adds the install of the whole card to
the BeagleBone Black's eMMC, which is the same layout written from one device
to another instead of over the network.

**Install to the internal storage (M11).** `wa-transcriber -flash /dev/mmcblk1`
(confirmed with `-flash-confirm /dev/mmcblk1`, the full device path or its
bare name — what the refusal prints is exactly what to type) and the System
page's "Internal storage" card behind the same typed name do the same thing,
and they need no network: the card *is* the image. The target is found by
rule, never by guess — the parent disk of `/sys/class/block/*` whose
`device/type` is `MMC`, that is not the disk the running root came from
(matched by device number, as `FindSlots` does) and that covers the bytes to
be written. The type is the primary discriminator; the `removable` flag is
not, because on the BeagleBone Black both the SD slot and the eMMC report
`removable=0` (only `device/type` = `MMC` plus "not the running disk" tells
them apart there), so it is only printed in the refusal line. Zero candidates
is "no internal storage" (the Pi 2's one line), two or more is a refusal that
lists what was seen, and a device the owner names has to pass the same checks.
What is copied is the MBR and the three system partitions — p1 boot
(MLO/U-Boot, kernel, boot file, `wat.txt`), p2 root A, p3 root B — each at the
offset it already has, because the partition table is copied with them; DATA
is *not* cloned (it is created on the new disk by `S01data` as on a fresh card
and filled from what the install staged), and nothing after p3 is written, so
a 400 MB card fits a 4 GB eMMC. Every region is hashed while it is read and
again as it is written, the write is fsynced, and then the destination is read
back and both sides re-hashed before anything says "remove the card and
reboot": a read-back that differs is the one check that separates "the
internal storage holds a copy" from "it will boot", and an eMMC that silently
wrote different bytes would only be found at the next power on otherwise. No
success is reported for a partial copy: a cancelled or failed clone names the
region that was not written, leaves the card untouched and the target
half-written — not bootable yet — and running it again completes it, since
every byte is a copy. The card is also the recovery path: the AM335x ROM boots
from the SD card when one is inserted, so the device keeps working whatever
state the eMMC is in.

Last, the session is *staged on the target's* boot partition (p1, just
cloned), not named by device: the install mounts `<target>p1` read-write,
copies the files that make the
device itself — `config.json`, `session.db` with `-wal`/`-shm`, `wifi.json` —
into `<boot>/wat-migrate.d/`, each through a temp name, fsync and rename, and
only then writes `wat-migrate` atomically beside them: one `file` line per
staged file. This is the fix for the one thing the first design got wrong: the
first boot from the eMMC is the one *after* the owner removed the card, and
then the card's DATA partition the old marker named (`from /dev/mmcblk0p4`)
does not exist, so the marker sat there forever and the device booted
unpaired.

Four things this had to learn on the board, each now pinned by a test:

- the clone copies the card's **boot file verbatim**, and that file names the
  root by device path (`root=/dev/mmcblk0p2`). Booted from the eMMC the same
  partition is `/dev/mmcblk1p2`, so the kernel found no root, panicked before
  userspace ran, and the board had neither network nor USB console — the one
  failure that looks like a dead board. The install now rewrites `root=` in
  the target's boot file (disk name only, partition number kept, rest of the
  file byte-identical, temp name + rename) before writing the marker, and
  reports the token it wrote;

- the kernel still holds the target's **old** partition table after the clone,
  so `<target>p1` is unmountable (`EINVAL`) until `partx -u <disk>` makes the
  kernel re-read it — hence the re-read seam, best effort, before the mount;
- the temp name of a staged file must be legal on **VFAT**:
  `.staging:config.json` failed with EINVAL and staged nothing, so the prefix
  is `wat-staging-`;
- the staging mount is remounted read-only and then **detached** with
  `unmount(2)`; a remount alone left `<target>p1` mounted, the next install
  then refused (it must not write under a live mount), and the FAT stayed
  writable for no reason. The remount call also passes its arguments in the
  documented `(dir, "", "remount,ro")` form, which the default implementation
  used to turn into `mount -o remount,ro <dir> ""`.

Every step — clone per region, read-back, table re-read, mount, each file with
its size and duration, marker, unmount — goes through `Request.Logf`, which is
how a serial console shows where an install is; the `-flash` mode wires it to
the console logger at INFO.

**The install is not tied to the page that starts it.** The System page's
button is an ordinary POST, and the first version ran the clone inside that
request: on a phone, the screen turning off (or any reload) cancelled the
request context and the copy stopped halfway, with the page gone and nothing
on screen to say so. The handler now runs the install on
`context.WithoutCancel(r.Context())` with the 30-minute bound, so losing the
browser cannot leave a half-written internal storage; the service keeps the
last progress (`ProgressDone`/`ProgressTotal`, updated per chunk, the copy
being the first half of the total and the read-back the second) and the
outcome (`Done`/`DoneAt`), and `Status` reports them, so a page loaded in the
middle of an install shows "copying · 192 MB of 412 MB · 47 %" or "checking
the copy", and one loaded afterwards shows the timestamp and what to do next.
While an install runs the card marks itself `data-flash="busy"` and `app.js`
reloads the page every 5 s, which is the whole polling mechanism — no new
endpoint, and the log still has every 16 MB line. Same reasoning as the
`!reset` re-exec: a long operation belongs to the device, not to a connection.

Two more from the first real migration, both in `S01data`:

- **`/data` may already be mounted** when the script runs (finishing a
  migration from a shell, or a second run in one boot). It now asks
  `mountpoint` first; before, mount answered `EBUSY` and the repair path ran
  `e2fsck` on a live filesystem, which refuses, so the migration silently did
  not happen;
- the **marker is removed after `/boot` is made writable**, not before: `rm`
  on the read-only boot partition failed while the log said the marker was
  gone, so a migrated device kept re-copying on every boot. The cleanup
  remounts `/boot` rw, removes the marker and the staged files, checks each
  removal, remounts ro, and logs exactly which of those happened.

The free space on the FAT is checked with statfs before the first
byte, with the cluster rounding; if the files do not fit, the clone is still
finished and reported as written and verified, and the message says the
session could not be staged (needs N, M free) and that the device will boot
unpaired with its pairing left on the card. The `from <device>` line is still
written and still read, for a board whose first boot happens with the card
inserted, but nothing depends on it. On the first start from the eMMC, after
`S01data` has created and mounted the new DATA, the script copies each listed
file from `/boot/wat-migrate.d/` (falling back to the `from` device for a file
that is not staged) to a temp name in `/data`, fsyncs and renames it, and
deletes the marker only when all of them are in; then it deletes
`wat-migrate.d` with `/boot` writable for a moment (if that remount fails the
copy already stands and the leftovers are a few kB). A power cut at any point
leaves the marker in place and the next boot copies everything again onto
identical files, and no source is ever written. Without the marker the device
boots unpaired, which is the only cost of an interrupted migration and not of
an interrupted clone.

## 12. Init mode (D19)

The image can start the transcriber as the machine's init instead of starting
busybox init and letting `/etc/inittab` respawn it. The bootloader says so:

```
append root=/dev/mmcblk0p2 … init=/usr/bin/wa-transcriber
```

The BeagleBone Black's `extlinux.conf` ships that token; the Raspberry Pis do
not yet (`cmdline.txt` needs its own first-boot story for `/data`, see the
bead). Everything the old boot path did in shell is done by `internal/boot`
before the rest of the program starts, in this order:

| Step | Was | Why the order |
|---|---|---|
| `mount /proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm` | `fstab` + `mount -a` in `rcS` | the memory size, the sysfs device walk and every block node need them |
| `mount` the tmpfs set (`/tmp`, `/run`, `/var/log`, `/var/lib`, `/var/lib/misc`) | same | `/etc/resolv.conf` is a symlink into `/tmp`: a DHCP client cannot write a name server before it exists |
| `Sethostname` from `/etc/hostname` | `hostname -F` | nothing else would do it |
| `/boot` = p1 of the running root's disk, vfat, `ro` | `S00boot` | `wat.txt` (the setup AP key) and the image updater read it |
| `modprobe bcm2835_wdt` / `omap_wdt` | `S90watchdog` | the watchdog has to be openable before the app arms it |
| `/data` = the partition labelled `DATA` from p4 up, created with `sfdisk`/`mkfs.ext4` on a card that has none | `S01data` | the session and the settings live there, and the boot gate re-execs a binary from it |
| the opt-in debug shell: `getty` on the UART, on tty1, or on a CDC-ACM gadget the program itself creates through configfs | `S02console` | the USB console exists only while a userspace program makes it. Booting as init without this step would take the console away exactly when a change to the boot path is being tested; it is off unless `console=` in `wat.txt` says otherwise, and it is the one place where the init path still starts busybox |

The rules of the shell script survive the port because they are what makes a
power cut during the first boot harmless: an existing `DATA` filesystem is
never touched, a partition carrying a filesystem this cannot name is left
alone (the device runs without `/data` rather than formatting a stranger), and
the two root slots are never candidates whatever their label. `blkid` is not
needed either: the label and the filesystem kind are read from the superblock
bytes.

The settings `/etc/inittab` used to carry are `initDefaults` in
`cmd/wa-transcriber/initmode.go`, and the kernel command line overrides any of
them with `wat.<flag>=<value>` (`wat.network=none` is the useful one on a
bench). PID 1 alone does not switch the mode on — a container has that too —
the command line has to name the binary as init.

Two things follow from being PID 1:

- **A failed setup is not a reboot loop.** If `boot.Setup` cannot bring the
  machine up, the program logs the reason to `/dev/kmsg` and `exec`s
  `/sbin/init`: busybox init reads `/etc/inittab`, runs `rcS` and the device
  comes up the old way, with a console. Everything `Setup` does is idempotent,
  so the old scripts find their filesystems where it left them. A board with
  no console access and no card reader is a board nobody experiments on.
- **Exit is a kernel panic.** `boot.Crash` writes the reason to `/dev/kmsg`,
  which the kernel prints on every active console, remounts nothing, reboots,
  and — if the reboot is refused — blocks with the watchdog still unpetted, so
  the hardware resets within ~20 s either way.
- **Every child process is ours to reap.** `wpa_supplicant`, `hostapd` and
  `dnsmasq` daemonise: the process that ends up as a child of init is not the
  one that was started and nobody waits for its exit. A blanket `wait4(-1)`
  goroutine is not the answer — it steals the exit status of the children
  `os/exec` is waiting for, and their `Wait` then fails with "waitid: no child
  processes" (measured: 2 of 200 `cmd.Run()` calls on Go 1.26). So
  `internal/proc` records every pid the program starts, and a sweep every
  second reaps only the zombies whose pid it never recorded. Every launch site
  goes through that table (`proc.Run`, `proc.Start`/`proc.Wait`,
  `proc.CombinedOutput`).
