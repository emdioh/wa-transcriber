# wa-transcriber

A small static binary that links to your WhatsApp account as a companion
device and transcribes voice notes. Built for headless boards (Raspberry Pi 2,
BeagleBone Black) and Docker.

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): architecture and the decisions behind it
- [docs/STATUS.md](docs/STATUS.md): what is done and verified, known limitations
- [docs/PLAN.md](docs/PLAN.md): remaining milestones, step by step, with exit criteria

Status: milestones 1, 3, 4, 5 (code), 6 (build side) and 7 (CI, v0.1.0
release and multi-arch image; container runtime checks still pending) — see
[STATUS.md](docs/STATUS.md).

## Build

Requires Go 1.26 (downloaded automatically by the `go` command if older).

```bash
make build        # ./wa-transcriber for this machine
make dist         # dist/: linux armv7 (Pi 2, BeagleBone Black), arm64, amd64
make test
```

## Run

```bash
./wa-transcriber -data ./data
```

Open the web UI (`http://<device>/`, or `-listen :8080` on a dev machine),
choose a password and link WhatsApp from the WhatsApp page. A QR code is also
printed in the terminal: WhatsApp → Settings → Linked devices →
Link a device. To pair with a code instead (no camera needed):

```bash
./wa-transcriber -data ./data -pair-phone +393331234567
```

Docker:

```bash
docker build -t wa-transcriber .
docker run -d --restart unless-stopped -p 8080:8080 -v wa-data:/data wa-transcriber
```

or the published image, built for amd64, arm64 and armv7:

```bash
docker run -d --restart unless-stopped -p 8080:8080 \
  -v wa-data:/data ghcr.io/emdioh/wa-transcriber
```

The container runs as uid 65532 and listens on `:8080` (a non-root process
cannot bind `:80`). A fresh named volume gets the right ownership; for a bind
mount run `sudo chown -R 65532:65532 <dir>` first, otherwise the container
cannot create its session. `docker inspect` shows the health status: the
image probes `GET /healthz` with `-healthcheck`, which works without a
session and also while the setup access point runs.

On Docker the network manager stays off (`WAT_NETWORK=none`), so the device
cannot discover the address clients use. Set `public_url` in `config.json`
to have `!status` list it first:

```json
{ "public_url": "https://wa.example.com" }
```

It must start with `http://` or `https://`.

| Flag | Env | Default |
|---|---|---|
| `-data` | `WAT_DATA` | `/data` |
| `-pair-phone` | `WAT_PAIR_PHONE` | (QR) |
| `-log-level` | `WAT_LOG_LEVEL` | `info` |
| `-listen` | `WAT_LISTEN` | `:80` (empty: no web UI) |
| `-watchdog` | `WAT_WATCHDOG` | empty (disabled); the images pass `/dev/watchdog` |
| `-coord-claims` | `WAT_COORD_CLAIMS` | off; see [Multi-transcriber](#multi-transcriber) |
| `-healthcheck` | — | probe the local UI and exit 0/1 (used by the image's `HEALTHCHECK`) |
| `-network` | `WAT_NETWORK` | `none` (`managed` on the image) |

## Flashing an SD card

Images for the Raspberry Pi 2 and the BeagleBone Black are built with
Buildroot ([os/README.md](os/README.md)):

```bash
make -C os check          # validates the build tree, a few seconds
make -C os rpi2           # os/_output/rpi2/images/sdcard.img
make -C os bbb            # os/_output/bbb/images/sdcard.img
sudo dd if=os/_output/rpi2/images/sdcard.img of=/dev/sdX bs=4M conv=fsync status=progress
```

Before ejecting, edit `wat.txt` on the small FAT partition (`setup_ap_key`,
`country`). First boot: the board publishes `WA-Setup-XXXX`, a phone joins
it and the captive portal opens the web UI; then link WhatsApp and add your
home WiFi. The root filesystem is read-only and `/data` is created on first
boot, so power cuts are safe. Reflashing erases `/data`: back up
`session.db*` and `config.json` first (without the session you must pair
again).

## Network

With `-network managed` the binary owns the network interfaces (the image
uses this; Docker and dev machines keep `none`):

- **Ethernet**: DHCP, or a link-local address (169.254/16) when there is no
  DHCP server, so a direct cable to a laptop works.
- **WiFi**: networks saved from the Network page; the passphrase is turned
  into the WPA PSK immediately and never stored.
- **Setup access point**: `WA-Setup-XXXX` (WPA2) with a captive portal, so a
  phone that joins it lands on the web UI. The key comes from
  `setup_ap_key=` in `wat.txt` on the boot partition; without the file a
  fallback key `wa-setup-<6 hex>` derived from the MAC is used (a warning is
  logged).
- **`wa.local`** resolves on the LAN through mDNS.
- `!reset` also forgets the saved WiFi networks.

## Multi-transcriber

When several people in a chat run wa-transcriber, each instance decides
whether to transcribe a voice note:

- every transcription reply carries an invisible tag, so the others skip a
  note that is already done;
- once the M2 experiment confirms that an empty reaction is invisible on all
  clients, `-coord-claims` (or `WAT_COORD_CLAIMS=1`) lets instances claim a
  note before working on it, with a hash-based order and a watchdog that
  takes over if the claimer disappears.

Until then the default is observe-only: no claims, duplicates possible but
rare.

## Transcription settings

Set them on the web UI's Transcription page, in `<data>/config.json` (read
at every voice note, no restart needed) or with environment variables, which
win over both, are never saved, and show as read-only in the UI:

```json
{
  "transcription": {
    "global": false,
    "provider": "openai",
    "api_key": "sk-...",
    "keep_languages": ["it", "en"],
    "translate_to": "it"
  }
}
```

| config.json | Env | Default |
|---|---|---|
| `provider` | `WAT_PROVIDER` | `openai` (also `openrouter`, `gemini`) |
| `api_key` | `WAT_API_KEY` | — required |
| `model` | `WAT_MODEL` | `gpt-4o-mini-transcribe` / `openai/whisper-large-v3` / `gemini-flash-latest` |
| `base_url` | `WAT_BASE_URL` | the provider's public API |
| `translation_model` | `WAT_TRANSLATION_MODEL` | `gpt-4o-mini` / `openai/gpt-4o-mini` (Gemini: same call) |
| `keep_languages` | `WAT_KEEP_LANGUAGES` (comma separated) | none |
| `translate_to` | `WAT_TRANSLATE_TO` | none (never translate) |
| `timeout_seconds` | `WAT_TIMEOUT` | 120 |

With `translate_to` set, a voice note in any language that is not in
`keep_languages` is posted as `🎤 [de→it] <translation>` followed by
`🗣 <original>`.

## Commands

Only messages **you** send are commands.

| Command | Where |
|---|---|
| `!t` (reply to a voice note) | any chat |
| `!transcription [global] [on\|off]` | any chat |
| `!status`, `!password [new]`, `!reset`, `!help` | only "Message yourself" (or a group with only you) |
