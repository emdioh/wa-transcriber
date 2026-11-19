# Status

Last update: 2026-11-16.

## Milestones

| # | Milestone | State |
|---|---|---|
| M1 | WhatsApp skeleton: pairing, message filter, commands, config, RAM logs, Dockerfile | ✅ done |
| M2 | Experiment: invisible coordination signals on real phones | ⏳ not started (needs two accounts) |
| M3 | Transcription + translation (OpenAI, OpenRouter, Gemini) | ✅ done |
| M3b | Multi-transcriber coordination protocol | ⏳ blocked by M2 |
| M4 | Web UI | ✅ done |
| M5 | Network manager (ethernet, WiFi, setup AP, mDNS, clock) | ⏳ not started (needs the Pi / Cubieboard + WiFi dongle) |
| M6 | Buildroot images (Pi 2, BeagleBone Black) | ⏳ blocked by M5 |
| M7 | CI, Docker image, releases | ⏳ not started (no hardware needed) |
| M8 | Small items (reply language, public URL in UI, health check) | ⏳ not started |

## Verified on real devices (by the owner)

- Pairing with WhatsApp and unlinking.
- Automatic and on-demand transcription posted as a quoted reply.
- `!status` replies, including the web UI URLs with port.

## Covered by unit tests only (not yet seen working against the real service)

- Translation flow and language detection, for each provider.
- Gemini and OpenRouter specifically (whichever provider the owner did not use).
- Pairing by phone number (code) from the web UI.
- `!password` deleting the command message for everyone.
- `!reset` end to end (logout + erase + re-exec).
- Self-chat detection for single-member groups.
- The Docker image (never built: no Docker in the development sandbox).

## Known limitations and accepted issues

- **`!status` links are not clickable**: WhatsApp does not linkify
  `http://<ip>:<port>/`. URLs are shown as inline code so they can be copied
  with a long press. Accepted. (A `nip.io` hostname was considered and
  rejected: external DNS dependency, blocked by DNS-rebinding protection on
  many routers, leaks the LAN IP.)
- The web UI is plain HTTP (no domain for a certificate). Do not expose it
  to the Internet.
- Until a password is set, whoever opens the UI first sets it. Set it right
  away (or with `!password` from WhatsApp).
- Logs are lost on restart (RAM only, by design).
- Bot replies are English only (M8 adds a setting).
- Two transcribers in one chat both answer (M3b fixes it).
- One WhatsApp account per device.

## Code map

```
cmd/wa-transcriber/main.go   flags, wiring, reset/unlink/re-exec, memory limit
internal/wa/                 whatsmeow client, pairing, event conversion, sending
internal/bot/                commands, voice dispatch, !status text
internal/transcribe/         settings resolution, providers, translation, queue
internal/web/                server, sessions, pages, templates/, static/
internal/config/             config.json store
internal/status/             runtime state, interface addresses
internal/logbuf/             RAM log buffer
docs/                        ARCHITECTURE.md, STATUS.md, PLAN.md
```

Build and test: `make test`, `make build`, `make dist`. Run locally:
`./wa-transcriber -data ./data -listen :8080`.
