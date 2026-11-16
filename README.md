# wa-transcriber

A small static binary that links to your WhatsApp account as a companion
device and transcribes voice notes. Built for headless boards (Raspberry Pi 2,
BeagleBoard) and Docker. Design: [docs/DESIGN.md](docs/DESIGN.md).

Status: milestone 1 — pairing, message handling, `!commands`, config,
in-memory logs. Transcription providers, web UI and network management are
next.

## Build

Requires Go 1.26 (downloaded automatically by the `go` command if older).

```bash
make build        # ./wa-transcriber for this machine
make dist         # dist/: linux armv7 (Pi 2, BeagleBoard), arm64, amd64
make test
```

## Run

```bash
./wa-transcriber -data ./data
```

On first start a QR code is printed: WhatsApp → Settings → Linked devices →
Link a device. To pair with a code instead (no camera needed):

```bash
./wa-transcriber -data ./data -pair-phone +393331234567
```

Docker:

```bash
docker build -t wa-transcriber .
docker run -it -v wa-data:/data wa-transcriber
```

| Flag | Env | Default |
|---|---|---|
| `-data` | `WAT_DATA` | `/data` |
| `-pair-phone` | `WAT_PAIR_PHONE` | (QR) |
| `-log-level` | `WAT_LOG_LEVEL` | `info` |

## Commands

Only messages **you** send are commands.

| Command | Where |
|---|---|
| `!t` (reply to a voice note) | any chat |
| `!transcription [global] [on\|off]` | any chat |
| `!status`, `!password [new]`, `!reset`, `!help` | only "Message yourself" (or a group with only you) |
