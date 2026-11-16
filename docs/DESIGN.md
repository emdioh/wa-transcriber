# wa-transcriber — design

A single static Go binary that links to a WhatsApp account as a companion
device and transcribes voice notes, driven by a handful of `!commands` and a
small web UI. It runs on a minimal Linux image (Raspberry Pi 2, BeagleBoard,
any ARMv7/ARM64/amd64 box) or in a Docker container.

## 1. Goals and non-goals

Goals:

- transcribe WhatsApp voice notes (auto per chat/global, or on demand with `!t`)
  and post the text as a quoted reply from the user's own account;
- optional translation: languages to keep as-is, a target language for the rest;
- control via `!commands` sent by the account owner only;
- web UI at `http://wa.local`: status, WhatsApp pairing, models, WiFi, logs;
- boots on a headless board and is always reachable: LAN + mDNS, WiFi client,
  or a setup access point;
- minimal: one binary, no Docker/Node/Chromium/Python/ffmpeg on the device;
  low SD-card wear; fits in 128–256 MB of RAM.

Non-goals: being a chat client, Signal/Telegram, local (on-device) speech
recognition in v1, multi-account.

## 2. Architecture

```
wa-transcriber (one process)
├── wa         whatsmeow client; session in /data/session.db (SQLite, pure Go)
├── bot        message filter + !commands + voice note dispatch
├── transcribe provider clients (OpenAI / OpenRouter / Gemini) + translation
├── coord      multi-transcriber election in shared chats (§6)
├── web        embedded HTML UI on :80 (password protected)
├── netmgr     ethernet / WiFi client / setup AP / mDNS (mode "managed"),
│              or nothing at all (mode "none": Docker, dev machines)
├── config     /data/config.json, atomic writes
└── logbuf     in-memory ring buffer (+ stderr)
```

whatsmeow is the pure-Go implementation of the WhatsApp multi-device
protocol. The SQLite driver is `modernc.org/sqlite` (no cgo), so the binary
cross-compiles with `GOOS=linux GOARCH=arm GOARM=7` and is fully static.

History sync is not needed (only new messages matter): the device asks for
the minimum and ignores history sync notifications, which keeps pairing light
on 128 MB boards.

## 3. Message handling

- Only messages younger than 10 minutes and not older than process start
  minus 60 s are handled (reconnections replay offline messages; history is
  never processed). Edits are ignored.
- **Voice note** (`AudioMessage` with `PTT`, or Ogg/Opus audio): if the
  effective setting of the chat is on, transcribe and reply quoting it.
- **Command**: a text message sent *by the account owner* (`IsFromMe`) that
  starts with `!`. Messages from anyone else are never commands. Unknown
  commands are ignored silently.
- Event handlers never block whatsmeow: work runs in goroutines.
- Replies never start with `!`, so the bot cannot trigger itself.

### Settings

- per-chat override `on`/`off`/unset, keyed by chat JID (both the phone-number
  and the LID form are written when known, so the two addressing modes agree);
- global default; effective = override if set, else global.

## 4. Commands

| Command | Where | Effect |
|---|---|---|
| `!t` (reply to a voice note) | any chat | transcribe that note |
| `!transcription` | any chat | show global/chat/effective state |
| `!transcription on\|off` | any chat | override for this chat |
| `!transcription global on\|off` | any chat | global default |
| `!status` | self only | status report incl. `http://<ip>` |
| `!password [new]` | self only | set the web UI password (random if omitted); the command message is deleted for everyone |
| `!reset` → `!reset confirm` (60 s) | self only | unlink WhatsApp, erase settings, password and WiFi networks, restart in setup mode |
| `!help` | self only | list commands |

"Self only" = the "Message yourself" chat, or a group whose only member is the
owner. Elsewhere these commands are ignored, so nothing leaks to contacts
(e.g. the device IP).

## 5. Transcription and translation

- Providers: OpenAI-compatible `/audio/transcriptions` (OpenAI, OpenRouter)
  and Gemini. Voice notes are Ogg/Opus and are uploaded as `.ogg`: no ffmpeg.
- Settings: provider, API key, model, base URL, timeout.
- Translation: `keep` languages (e.g. `it, en`) and `translate_to` (e.g. `it`).
  The transcript's language is detected; if not in `keep`, a chat model
  translates it and the reply is `🎤 [de→it] <translation>` followed by the
  original. With Gemini a single audio call can do all of it.
- One job at a time (bounded queue); audio streamed from disk to the API.

## 6. Several transcribers in one chat

Messages are end-to-end encrypted, so the server cannot strip extra protobuf
fields; official clients ignore them. Two binaries can therefore talk through
fields humans never see:

- **Tag**: every transcription reply carries `ContextInfo.conversionSource =
  "wat/1"`, so peers recognise each other's transcriptions.
- **Claim**: an *empty reaction* to the voice note (= "remove my reaction",
  invisible when there was none) with `groupingKey = "wat/claim"`.

Protocol per voice note:

1. the voice note's sender claims immediately; everybody else waits a
   deterministic 1–4 s derived from `hash(msgID + ownJID)`;
2. if a claim arrived meanwhile: stand down and start a watchdog (≈45 s + note
   length); if no tagged transcription shows up, take over;
3. otherwise claim and transcribe. Crossing claims: lowest
   `hash(msgID + JID)` wins, everybody computes the same answer.

v1: the winner's translation settings apply. Legacy bots (Megachat's
`🎤 Trascrizione:` prefix) are recognised after the fact only.

**To be verified first** (milestone 2): empty reactions are invisible on
Android, iOS and Web, and the extra fields survive delivery.

## 7. Web UI

Server-rendered HTML embedded in the binary, no JS framework. Pages: Status,
WhatsApp (QR that refreshes itself, or pairing code by phone number),
Transcription, WiFi, Logs. Password protected (bcrypt hash in config; set on
first setup or with `!password`).

## 8. Networking (mode "managed")

Interfaces are handled independently:

- **Ethernet**: on carrier, DHCP; if nobody answers, IPv4 link-local
  (169.254/16), so a direct cable to a laptop works. mDNS `wa.local` is
  announced as soon as an address exists.
- **WiFi** (if an adapter exists): try saved networks (~45 s); otherwise start
  the setup AP `WA-Setup-XXXX` (WPA2) with DHCP and a DNS that answers every
  name with the device's address (works without mDNS and triggers the phone's
  captive portal page). While in AP mode and nobody is connected, saved
  networks are retried periodically. A scan is cached before switching to AP.
- **AP policy**: `auto` (default: AP up while not paired or while no other
  network works), `always`, `never`. The AP never routes to the LAN.
- Tools driven: `wpa_supplicant`, `hostapd`, `dnsmasq` (DHCP + DNS), `udhcpc`.
  mDNS is implemented in the binary.
- The clock must be synced (NTP) before connecting to WhatsApp; status shows
  "clock not synced" otherwise.

Mode "none" (Docker, development): no network management, no mDNS unless
`--network host`; `!status` shows a configured public URL.

## 9. Storage and logs

- `/data`: `session.db`, `config.json`, WiFi networks. Root filesystem
  read-only on the image; `/tmp` and `/var/log` are tmpfs.
- Logs live in RAM only: a ring buffer of ~1% of total memory, clamped to
  256 KB–4 MB, shown in the UI and downloadable. They are lost on reboot.

## 10. Packaging

- Binary: `make build-arm7`, `build-arm64`, `build-amd64`.
- Docker: static binary + CA certificates + tzdata on `scratch`, multi-arch.
  `!reset` re-executes the process (restart policy not even needed).
- Images: Buildroot, one board config per target (Pi 2, BeagleBoard), same
  userspace.

## 11. Milestones

1. whatsmeow skeleton: pairing (terminal QR / pairing code), message filter,
   commands, config, in-memory logs, Dockerfile.
2. Invisible claim experiment (§6) on real phones.
3. Transcription + translation, then the coordination protocol.
4. Web UI.
5. netmgr + mDNS.
6. Buildroot images (Pi 2 first, then BeagleBoard).
