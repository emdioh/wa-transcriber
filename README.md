# wa-transcriber

A small static binary that links to your WhatsApp account as a companion
device and transcribes voice notes. Built for headless boards (Raspberry Pi 2,
BeagleBoard) and Docker. Design: [docs/DESIGN.md](docs/DESIGN.md).

Status: milestones 1 and 3 — pairing, `!commands`, transcription and
translation (OpenAI, OpenRouter, Gemini), config, in-memory logs. Web UI and
network management are next.

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

## Transcription settings

Until the web UI exists, set them in `<data>/config.json` (read at every
voice note, no restart needed) or with environment variables, which win over
the file and are never saved:

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
