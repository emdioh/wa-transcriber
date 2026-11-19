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
  - CSP `default-src 'self'`, no inline scripts, no framing;
  - the API key is write-only (never rendered back).

## 6. Multi-transcriber coordination (planned — milestones 2 and 3b)

Problem: two people in the same chat both run a transcriber → every voice
note is transcribed twice. Constraint: no visible "bot chatter".

Idea: messages are end-to-end encrypted, so extra protobuf fields reach the
other side untouched, and official clients ignore what they do not render.

- **Tag**: every transcription reply carries a marker (candidate:
  `ContextInfo.conversionSource = "wat/1"`; final field chosen by the
  milestone 2 experiment).
- **Claim**: an *empty reaction* (= "remove my reaction", invisible when
  there was none) on the voice note, with `groupingKey = "wat/claim/1"`.

Per voice note: the sender's own transcriber claims at once; everybody else
waits a deterministic 1–4 s derived from `sha256(voiceID|ownNumber)`; a claim
seen while waiting → stand down and arm a watchdog (45 s + note length) that
takes over if no tagged transcription appears; crossing claims → lowest hash
wins. `!t` bypasses coordination. Details and fallbacks: PLAN.md, M2/M3b.

## 7. Networking (milestone 5)

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

- **Ethernet**: on carrier up, `udhcpc -n` for 15 s; no lease → an IPv4
  link-local address that passed 2 ARP probes (`arping -D`), retried from a
  MAC hash on collision; DHCP is retried every 60 s and the link-local
  address is dropped on success. Carrier down → `ip addr flush`.
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
  interface's IPv4 (TTL 120, cache-flush), unicast when the query asks for
  it; a conflicting responder makes it rename to `wa-2.local`, `wa-3.local`.
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
