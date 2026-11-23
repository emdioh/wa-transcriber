# Implementation plan for the remaining milestones

Audience: an engineer joining the project. Read
[ARCHITECTURE.md](ARCHITECTURE.md) first (decisions D1–D15 explain *why*
things are the way they are; do not change them without discussing it) and
[STATUS.md](STATUS.md) (what already works).

Each milestone has:

- **Gate in**: what must be true before starting.
- **Steps**: in order, each small enough for one pull request.
- **Exit criteria**: all must hold before the milestone is called done.
- **Gate out**: the decision taken at the end and what it unlocks.

## Order

```
M7 CI/Docker ──► M8 small items

M2 experiment ──► M3b coordination (observe-only until M2 decides)

M5 network manager (code, done) ──► M6 Buildroot images
                                          │
M5b network validation (needs hardware) ──┴──► v1.0
```

M7 first: it needs no hardware and protects every later change. M2/M3b,
M5/M6 and M5b are independent tracks: M6 only needs the network-manager
*code*, not its hardware validation, so images are developed and built first
and validated on the boards in M5b. Do the tracks in whatever order the
hardware and the test phones become available.

M9 (binary updates) rides on M7's releases and M6's images: its code and tests
need no hardware, only the field test on the boards does. Its gate out is
v1.1, the first release whose image can update itself.

---

## Working rules (all milestones)

1. **Toolchain**: Go version from `go.mod` (1.26). `gofmt`, `go vet ./...`
   and `go test -race ./...` must pass on every commit.
2. **One step = one pull request**, with tests. Keep PRs under ~400 changed
   lines when possible.
3. **Tests never touch the network or real hardware.** Put every external
   effect behind a small interface (the code already does this:
   `bot.Messenger`, `bot.Transcriber`, `transcribe.Downloader`,
   `web.WhatsApp`) and test with fakes. Command execution and `/sys` reads in
   M5 follow the same rule (`Runner`, `FS` interfaces, see M5).
4. **Time is injected** (`Now func() time.Time`, a fake clock in tests). No
   `time.Sleep` in tests longer than a few milliseconds.
5. **Logging**: `log/slog` with key/value pairs, message in lower case,
   English. Never log secrets (API keys, passwords, WiFi passphrases, setup
   AP key). Errors that may contain a secret go through a redaction helper
   (see `transcribe.excerpt`).
6. **User-facing text** (WhatsApp replies, UI) is short and plain. Never
   start a bot reply with `!`.
7. **Persistent state** only through `config.Store.Update` (or a store built
   the same way: temp file + fsync + rename + dir fsync). Never write
   anything outside the data directory.
8. **Commit messages**: imperative subject ≤ 72 chars, body explains why.
9. **Docs**: update STATUS.md when a milestone or a verification changes;
   update ARCHITECTURE.md when a design detail changes.

Definition of done for every step: code + tests + docs updated, CI green,
reviewed by a second person.

---

## M7 — CI, Docker image, releases

Goal: every push is built and tested; a version tag produces binaries and a
multi-arch container image.

**Gate in**: none.

**Status (2026-11-20): CI green, v0.1.0 released.** `-healthcheck` and
`GET /healthz` exist and are tested (the probe also works while the setup AP
runs), the Dockerfile runs as uid 65532 on `:8080` with a `HEALTHCHECK`, and
CI runs gofmt, vet, `go test -race ./...` and `make dist` on every push. Tag
`v0.1.0` published the three binaries with `SHA256SUMS` and the multi-arch
image (`ghcr.io/emdioh/wa-transcriber`, tags `v0.1.0`/`0.1.0`/`latest`, now
private because the repository is private). Image sizes are now
logged per platform (`v0.1.1`: 9.1 MB amd64, 8.4 MB arm64, 8.7 MB armv7
compressed; `docker image ls` 24.5 MB), so the < 30 MB criterion holds. The container now runs
healthy on amd64 and shows the pairing QR (owner check); still open: an
end-to-end transcription, the `!reset` re-exec, restart keeping the pairing,
an arm host, and branch protection on `main`. One real-world catch found on
the owner's host: a volume created by the old root image cannot be written by
uid 65532, so startup now fails with the exact `chown` command instead of
serving a UI whose saves fail. Podman re-checked the whole flow (fresh
volume owner 65532, healthy, setup, real pairing, an OpenRouter
transcription, pairing survives a restart, root-owned volume fails fast) and
exposed a 20 GB Docker build context from `os/_output`, now excluded in
`.dockerignore`. The `!reset` re-exec is verified too (same container, restarts=0, back in
setup mode). Remaining: an arm host.

### Steps

1. **CI workflow** `.github/workflows/ci.yml`, on `push` and `pull_request`:
   - `actions/checkout`, `actions/setup-go` with `go-version-file: go.mod`;
   - check formatting: `test -z "$(gofmt -l .)"`;
   - `go vet ./...`, `go test -race ./...`;
   - `make dist`, upload `dist/` as a workflow artifact.
2. **Health endpoint**: add `GET /healthz` to `internal/web` (no auth,
   returns `200 ok`), and a `-healthcheck` flag in `main` that does
   `GET http://127.0.0.1:<port>/healthz` and exits 0/1. Tests: the route
   answers without a session; the flag returns 1 when nothing listens.
3. **Docker image hardening** (`Dockerfile`):
   - run as a non-root user: `USER 65532:65532`; create `/data` owned by
     that uid in the build stage (`mkdir -p /out/data && chown 65532:65532
     /out/data`, then `COPY --from=build --chown=65532:65532 /out/data /data`);
   - default `ENV WAT_LISTEN=:8080` (non-root cannot bind :80), `EXPOSE 8080`;
   - `HEALTHCHECK CMD ["/wa-transcriber","-healthcheck"]`;
   - add `import _ "time/tzdata"` in `main.go` and drop the zoneinfo copy.
4. **Release workflow** `.github/workflows/release.yml`, on tags `v*`:
   - build `dist/` with `VERSION=${GITHUB_REF_NAME}`, write
     `dist/SHA256SUMS`, attach everything to a GitHub release;
   - `docker/setup-buildx-action`, `docker/login-action` to `ghcr.io` with
     `GITHUB_TOKEN` (`permissions: packages: write`), `docker/build-push-action`
     for `linux/amd64,linux/arm64,linux/arm/v7`, tags `:vX.Y.Z` and `:latest`.
5. **Docs**: README "Docker" section with `docker run -d --restart
   unless-stopped -p 8080:8080 -v wa-data:/data ghcr.io/emdioh/wa-transcriber`,
   and `public_url` explained (see M8 step 2).

### Exit criteria

- CI is green on `main`; a PR with a gofmt error or a failing test fails CI.
- Tag `v0.1.0` produces a release with 3 binaries + `SHA256SUMS`, and an
  image on ghcr.io for 3 architectures.
- Image size < 30 MB (`docker image ls`).
- Manual test with the image on amd64 **and** on an arm64 or armv7 host:
  first-visit setup, pairing, one transcription, `!reset` (the container must
  come back in setup mode without being restarted by Docker, thanks to the
  re-exec), container restart keeps the pairing (volume).
- `docker inspect` shows the container healthy.

### Gate out

Turn on branch protection on `main` requiring CI. From now on nothing merges
red.

---

## M8 — Small items

**Gate in**: M7 done.

**Status (2026-11-20): done in code.** `language` (`en`/`it`) with
`internal/bot/i18n.go` and a completeness test, the language select on the
System page, `public_url` on the Network page when network management is off,
and one startup log line with the effective non-secret settings. Still open:
the manual Italian walkthrough on a phone.

### Steps

1. **Reply language**.
   - `config.Config.Language string` (`"en"` default, `"it"`).
   - `internal/bot/i18n.go`: `type msgKey int` constants for every reply
     string in `bot.go` and `statustext.go`; `var messages =
     map[string]map[msgKey]string{"en": {...}, "it": {...}}`; `func (b *Bot)
     t(key msgKey, args ...any) string` (falls back to English).
   - Replace every literal reply with `b.t(...)`.
   - Web: a "Language of the bot's replies" select on the System page.
   - Tests: a test that iterates every `msgKey` and fails if a language lacks
     it; existing tests keep passing with `"en"`.
2. **Public URL in the UI**: on the Network page, when network management is
   off (Docker), a field for `config.PublicURL` (validated: `http(s)://…`).
   Already shown by `!status` when set.
3. **Startup log of effective settings** without secrets: provider, model,
   languages, listen address, data dir, memory limit.

### Exit criteria

- With language `it`, every bot reply is Italian (manual check of `!help`,
  `!status`, `!transcription`, `!t` hint, `!reset` flow, password replies).
- The completeness test exists and passes.

### Gate out

None.

---

## M2 — Experiment: invisible coordination signals

Goal: find out, on real phones, whether two binaries can exchange signals
inside a chat **without any human noticing**. This is research: the output is
a results document and a decision, not product code.

**Gate in**:

- M7 done (to build two binaries easily).
- Two WhatsApp accounts **A** and **B** (B can be a second SIM / family
  member), each with its own phone, plus WhatsApp Web open for A and B on a
  computer. At least one Android and one iPhone among the phones.
- Two running instances: `wa-transcriber` linked to A, and another linked
  to B (separate `-data` directories, can be on the same computer with
  different `-listen` ports).

### Steps

1. **Experiment switch**: env `WAT_EXPERIMENTS=1` enables the code below;
   without it the code is unreachable. Put everything in
   `internal/bot/experiment.go` and `internal/wa/experiment.go` so it is easy
   to delete later.
2. **Sender side** — command `!probe` (owner only, works in any chat, only
   when experiments are on), sent **as a reply** to a voice note from B. The
   bot sends, 10 s apart, logging `probe-tx variant=… id=…`:
   - **V1** empty reaction to the voice note with `GroupingKey =
     "wat/claim/1"` (`cli.BuildReaction(chat, sender, id, "")` then set
     `ReactionMessage.GroupingKey`).
   - **V2** empty reaction without grouping key (control for V1).
   - **V3** a text reply `probe V3` quoting the voice note, with
     `ContextInfo.ConversionSource = "wat/1"`.
   - **V4** same, with `ContextInfo.ConversionData = []byte("wat/1")`.
   - **V5** same, with `ContextInfo.EntryPointConversionSource = "wat/1"`.
   - **V6** a plain text reply `probe V6` (control for V3–V5).
3. **Receiver side** — with experiments on, `wa.convert` logs every
   incoming message that carries a reaction or one of the fields above:
   `probe-rx variant=… field=… value=… latency_ms=<now − message timestamp>`.
   Reactions arrive as `*events.Message` whose `Message.ReactionMessage` is
   set: today `convert` ignores them (no text) — log them before returning.
4. **Observation matrix**. For each variant, record on **every** client of
   A and B (Android app, iPhone app, WhatsApp Web, desktop app if available),
   with the app in foreground **and** in background:

   | Observation | How |
   |---|---|
   | Anything visible in the chat (bubble, reaction badge, "deleted" placeholder, system line) | look at the chat |
   | Push notification | app in background, screen locked |
   | Chat jumps to the top of the list / unread badge | look at the chat list |
   | Any visual difference between V3–V5 and the control V6 | compare side by side (e.g. "from ad" labels) |
   | Field received by B's instance | `probe-rx` log lines |
   | Latency | `latency_ms` |

5. **Repetitions**: run the whole probe 10 times in the A–B private chat and
   10 times in a group with A, B and a third person without a transcriber.
6. **Write the results** in `docs/experiments/M2-results.md` with: date,
   app versions per device, the matrix (✔/✘ per cell), received counts per
   variant (x/20), latency p50/p95, screenshots of anything unexpected.

### Exit criteria

- The results document is complete for every variant × client × condition.
- A decision (below) is written at the end of the document and reviewed.

### Gate out — decision table

| Result | Decision for M3b |
|---|---|
| V1 invisible everywhere (incl. notifications, chat order), received ≥ 19/20, p95 latency < 2 s | **Claim = V1.** |
| V1 not acceptable | **No claims.** M3b runs in "observe-only" mode: deterministic delay, skip when a tagged transcription already exists; rare duplicates accepted. |
| At least one of V3–V5 is indistinguishable from V6 and received 20/20 | **Tag = the first such variant** (prefer V3). |
| None of V3–V5 acceptable | **Tag = visible prefix**: a reply quoting the voice note whose text starts with `🎤 ` is treated as a transcription. |

Whatever the result, remove the experiment code (or keep it behind the
switch, documented) before M3b merges.

---

## M3b — Multi-transcriber coordination

Goal: in a chat where several people run wa-transcriber, each voice note is
transcribed exactly once, without visible chatter, and never left
untranscribed because a peer disappeared.

**Gate in**: M2 decision recorded (claim variant or observe-only; tag
variant or prefix). **Without an M2 result the milestone can still start:**
implement the full coordinator but run it in **observe-only** mode (claims
disabled, deterministic delay, skip when a tagged transcription already
exists; rare duplicates accepted). The claim mode stays behind a switch until
M2 produces a usable variant, then it is turned on by default.

**Status (2026-11-19): done in observe-only mode.** `internal/coord` (rank,
delay, settle window, watchdog takeover), the wire format (`wat/claim/1`
grouping key, `wat/1` `ConversionSource` tag, legacy prefix), the bot
integration and the simulation tests are in. `-coord-claims` /
`WAT_COORD_CLAIMS=1` enables real claims; M2 decides whether it becomes the
default. The field tests (exit criteria below) still need two accounts.

### Design to implement

New package `internal/coord`:

```go
type Signals interface {
    // Claim publishes "I am transcribing voiceID" (no-op in observe-only mode).
    Claim(ctx context.Context, chat, voiceSender types.JID, voiceID string) error
}

type Coordinator struct {
    Self    func() []types.JID      // own phone-number and LID JIDs
    Signals Signals
    Now     func() time.Time
    After   func(time.Duration) <-chan time.Time // fake in tests
    Log     *slog.Logger
    // ClaimMode false = observe-only (M2 fallback).
    ClaimMode bool
}

// Acquire blocks until this instance should transcribe the voice note
// (returns a Lease) or another instance takes care of it (returns nil).
func (c *Coordinator) Acquire(ctx context.Context, v Voice) *Lease

// Lease.StillMine is called right before posting: false if a peer won
// meanwhile (crossing claims) or a tagged transcription already exists.
func (l *Lease) StillMine() bool

// Observe feeds every incoming claim and tagged transcription.
func (c *Coordinator) ObserveClaim(chat types.JID, voiceID string, from types.JID, at time.Time)
func (c *Coordinator) ObserveTranscription(chat types.JID, voiceID string, from types.JID)
```

`Voice` carries chat, voice ID, voice sender, whether the sender is us, and
the note length (`AudioMessage.Seconds`).

Algorithm per voice note (state keyed by `chat|voiceID`, kept 10 minutes):

1. `rank(jid) = first 8 bytes of sha256(voiceID + "|" + jid.User)` as
   uint64; lower wins. Use the phone-number JID when known.
2. Delay: 0 if we sent the voice note; otherwise `1 s + rank(self) % 3000 ms`.
3. While waiting: if a claim or a tagged transcription for this note
   arrives → stand down (go to 6).
4. After the delay: claim (claim mode), then wait a **settle window** of
   1.5 s; if a claim from a peer with a lower rank arrives in that window →
   stand down (go to 6).
5. Transcribe. Before posting call `StillMine()`; if false, drop the result
   (log `coord: dropped, peer won`).
6. Standing down arms a watchdog: `45 s + note length`. If no tagged
   transcription for the note arrives in time → take over: claim and
   transcribe (skip the delay).

`!t` bypasses the coordinator (the owner asked explicitly).

### Steps

1. **Wire format** (`internal/wa`): helpers to build the claim (M2 variant)
   and to set the tag on transcription replies (`wa.Client.ReplyTagged` or a
   `bot.Quote` option). `convert` recognises incoming claims and tagged
   replies (and the legacy Megachat prefix `🎤 Trascrizione:` quoting a
   voice note) and puts them in new `bot.Message` fields (`Claim *ClaimInfo`,
   `TranscriptionOf string`). Tests: build → convert round trip for each.
2. **`internal/coord`** with the algorithm above and table tests on a fake
   clock:
   - alone in the chat → proceeds after its delay;
   - own voice note → proceeds immediately;
   - peer claim during the delay → stands down;
   - crossing claims → exactly the lower rank proceeds;
   - peer claims then never posts → watchdog takes over at 45 s + length;
   - tagged transcription arrives while we transcribe → `StillMine` false;
   - observe-only mode → no `Claim` calls, still skips when a tagged reply
     exists.
3. **Simulation test**: N = 2..5 coordinators connected by a fake bus that
   delivers each signal to the others with a random latency of 0–1.5 s.
   1000 random runs per N. Assert: never zero transcriptions (after
   watchdogs); exactly one in ≥ 99% of runs in claim mode.
4. **Integration** in `bot.onVoice`: `lease := coord.Acquire(...)`; nil →
   return; transcribe; `if !lease.StillMine() { return }`; post tagged.
   `bot.Handle` routes claim/transcription messages to `Observe*` first and
   never treats them as commands or voice notes.
5. **Status**: `!status` and the Status page show
   `coordination: claim/observe-only, stood down N, took over M` (counters
   since start).

### Exit criteria

- Unit and simulation tests pass with `-race`.
- Field test (A and B both with transcription on in a shared group and in
  their private chat): 20 voice notes from A, 20 from B, 20 from a third
  person → **0 duplicates, 0 missing**.
- Field test with B's instance stopped: every note transcribed by A within
  the watchdog time.
- Nothing visible in any client beyond the transcriptions themselves.

### Gate out

Update ARCHITECTURE.md §6 with the final wire format. Coordination is on by
default (no setting).

---

## M5 — Network manager (code)

Goal: the device is always reachable: ethernet with DHCP or link-local,
saved WiFi networks, or its own setup access point; `wa.local` resolves on
every network; WiFi is configured from the web UI.

**Status (2026-11-19): done.** Steps 1–12 are implemented and unit-tested
with scripted fakes (`internal/netmgr`, `internal/netmgr/mdns`). The dongle
gate and the on-device scenarios moved to **M5b**.

**Gate in**: none (M4 done).

Development tip: most of this can be exercised on any Linux laptop with
simulated WiFi radios: `sudo modprobe mac80211_hwsim radios=2` creates
`wlan0` and `wlan1` that can talk to each other (one as AP, one as client).

### Package layout

```
internal/netmgr/
  netmgr.go     Manager interface, Status types, New(mode)
  none.go       mode "none" (Docker/dev): reports addresses only
  linux.go      mode "managed": orchestration (state machine)
  run.go        Runner interface: Run(ctx, name, args...) and Start (long-lived)
  sysfs.go      FS interface over /sys/class/net (fake in tests)
  ether.go      ethernet: carrier, DHCP, link-local
  wifi.go       wpa_supplicant client mode, scan
  ap.go         hostapd + dnsmasq setup AP
  store.go      /data/wifi.json (saved networks)
  mdns/         in-binary mDNS responder for wa.local
```

```go
type Manager interface {
    Start(ctx context.Context) error
    Status() Status               // per interface: kind, state, IP, SSID, AP clients
    Scan(ctx context.Context) ([]Network, error)   // cached during AP mode
    SaveNetwork(ssid, passphrase string) error     // then try it
    ForgetNetwork(ssid string) error
    SetAPPolicy(p APPolicy) error                  // auto|always|never
    Erase() error                                  // !reset
}
```

New flag `-network managed|none` (env `WAT_NETWORK`, default `none`; the
image sets `managed`). The binary must run as root in managed mode (D12;
it changes interfaces and binds port 53/67/5353).

### Steps

1. **Skeleton + mode none**: interfaces, `none.go` (wraps
   `status.Addresses`), flag, wiring in `main`; `web` Network page uses
   `Manager.Status()`. Tests for the flag and the none manager.
2. **Interface discovery** (`sysfs.go`): list `/sys/class/net/*`; skip `lo`
   and virtual ones (no `device` link); `wireless` subdir or `phy80211` →
   WiFi, otherwise ethernet. Carrier from `/sys/class/net/<if>/carrier`.
   Poll every 2 s (netlink is a later optimisation). Tests with a fake FS
   tree under `t.TempDir()`.
3. **Ethernet** (`ether.go`):
   - carrier up → `ip link set <if> up` → `udhcpc -i <if> -f -q -t 5 -T 3
     -s /usr/share/udhcpc/default.script` (the image ships its own
     idempotent script through the rootfs overlay; on Raspberry Pi OS
     install `udhcpc`);
   - no lease within 15 s → link-local: address `169.254.(h>>8 % 254 +1).(h % 254 + 1)`
     from a hash of the MAC; check with `arping -D -c 2 -I <if> <ip>`; on
     conflict hash again (max 10 tries); `ip addr add <ip>/16 dev <if>`;
     keep retrying DHCP every 60 s and drop the link-local address when a
     lease arrives;
   - carrier down → keep the address and stop DHCP; on the next carrier up
     DHCP runs again with the same command (no `-R`: that would release the
     address on exit) and our script keeps it unless the lease changed.
     Flushing is an explicit teardown only (`Ethernet.Flush`), decision
     D16, wat-f2z.9: a flush on a carrier blip kills live TCP connections.
   Tests: fake `Runner` scripted outputs; assert the exact command sequence.
4. **Saved networks** (`store.go`): `/data/wifi.json`, `0600`, atomic writes
   like `config`. Store `ssid`, `psk` as the 64-hex PSK derived with
   PBKDF2-SHA1(passphrase, ssid, 4096, 32) (same as `wpa_passphrase`; use
   `crypto/pbkdf2` from the standard library) so the passphrase is never stored,
   `last_ok` timestamp, `priority`. Test vector: compare with the output of
   `wpa_passphrase` for 2 SSIDs.
5. **WiFi client** (`wifi.go`):
   - write `/run/wat/wpa.conf` (`ctrl_interface=/run/wat/wpa`,
     `update_config=0`, one `network={ssid="…" psk=<hex> priority=N}` per
     saved network, most recent success = highest priority; escape SSIDs
     with `ssid=<hex>` form to avoid quoting bugs);
   - `wpa_supplicant -i wlan0 -D nl80211 -c /run/wat/wpa.conf` started with
     `Runner.Start` (supervised: restart if it dies);
   - poll `wpa_cli -p /run/wat/wpa -i wlan0 status` until
     `wpa_state=COMPLETED` (timeout 45 s), then DHCP as in step 3;
   - scan: `wpa_cli scan` + `scan_results` parsing (bssid, freq, signal,
     flags, ssid); dedupe by SSID keeping the strongest.
   Tests: parsers with real captured outputs (store them in `testdata/`).
6. **Setup AP** (`ap.go`):
   - stop wpa_supplicant; `ip addr flush dev wlan0`; `ip addr add
     192.168.4.1/24 dev wlan0`; `ip link set wlan0 up`;
   - `/run/wat/hostapd.conf`: `interface=wlan0`, `driver=nl80211`,
     `ssid=WA-Setup-XXXX` (XXXX = last 4 hex digits of the MAC),
     `hw_mode=g`, `channel=6`, `country_code=<from wat.txt, default IT>`,
     `ieee80211d=1`, `wpa=2`, `wpa_key_mgmt=WPA-PSK`, `rsn_pairwise=CCMP`,
     `wpa_passphrase=<setup key>`;
   - setup key: read from the boot partition file `wat.txt`
     (`setup_ap_key=…`, editable from any PC since the partition is FAT; M6
     creates it). If missing or shorter than 8 characters: use
     `wa-setup-` + last 6 hex digits of the MAC and log a warning. Document
     it on the label / README;
   - `dnsmasq --keep-in-foreground --no-resolv --no-hosts
     --interface=wlan0 --bind-interfaces
     --dhcp-range=192.168.4.10,192.168.4.100,255.255.255.0,1h
     --address=/#/192.168.4.1 --dhcp-option=114,http://192.168.4.1/`
     (option 114 = captive portal URL, RFC 8910);
   - stations: `iw dev wlan0 station dump` (count `Station` lines).
   Tests: generated config files compared with golden files in `testdata/`.
7. **Captive portal endpoints** (`internal/web`), active only in AP mode
   (ask the Manager): `GET /generate_204`, `/gen_204` (Android),
   `/hotspot-detect.html`, `/library/test/success.html` (Apple),
   `/connecttest.txt`, `/ncsi.txt` (Windows) → `302` to `http://192.168.4.1/`.
   Requests with any `Host` other than the device IP or `wa.local` → same
   redirect. Tests with `httptest`.
8. **Policy state machine** (`linux.go`): per WiFi interface the states
   `Off → Connecting → Connected | AP`; inputs: carrier/addresses from
   ethernet, WhatsApp paired (from `wa.Client.Paired`), saved networks,
   AP stations, timers. Rules (policy `auto`):
   - saved networks exist → `Connecting`; success → `Connected`;
   - `Connecting` failed (45 s) → `AP` if (not paired) or (no ethernet
     address); otherwise `Off` and retry in 5 min;
   - `AP` with 0 stations for 5 min and saved networks exist → scan; if a
     saved SSID is visible → `Connecting`;
   - `Connected` lost for 2 min → `Connecting` (wpa_supplicant retries by
     itself first);
   - `always`: AP stays up (client mode disabled); `never`: never AP.
   Write it as a pure function `next(state, inputs) (state, []action)` and
   test it exhaustively with a table (every state × every input), then a
   thin loop that executes actions through the Runner.
9. **mDNS** (`mdns/`): answer `A` queries for `wa.local.` on every
   interface with that interface's IPv4.
   - Use `golang.org/x/net/ipv4` to join `224.0.0.251:5353` per interface
     (`SetMulticastInterface`, `SetControlMessage` to know the receiving
     interface); parse with `golang.org/x/net/dns/dnsmessage`.
   - Answer questions for `wa.local.` type `A` or `ANY` (unicast-response
     bit → unicast reply), TTL 120, cache-flush bit set.
   - On address change: two unsolicited announcements 1 s apart.
   - Conflict: if another host answers for `wa.local` with a different IP,
     switch to `wa-2.local` (then `-3`…) and log it.
   - Tests: feed packets built with `dnsmessage` into the handler; assert
     the response bytes. Manual: `avahi-resolve -n wa.local` (Linux),
     `dns-sd -G v4 wa.local` (macOS), `ping wa.local` (Windows 10+), iOS
     browsers and Android 12+ with a Google Play System Update from
     November 2021 or later (which added one-shot mDNS queries to the DNS
     resolver); older Android only has `NsdManager` service discovery, so a
     `.local` name does not resolve there at all.
10. **Clock**: before `wa.Client.Run` connects, wait until the clock is sane:
    `time.Now()` later than the build time (embed it with
    `-ldflags -X main.buildTime=…`) **and**, in managed mode, `adjtimex`
    reports synced (`golang.org/x/sys/unix.Adjtimex`, `STA_UNSYNC` cleared)
    — give up waiting after 2 minutes and connect anyway with a warning.
    Status shows "clock not synced". The image runs busybox `ntpd` (M6).
11. **Web UI Network page**: interface table (kind, state, IP, SSID or
    "setup AP, N clients"), scan list with signal bars and "Connect"
    (passphrase prompt), saved networks with "Forget", AP policy select.
    POSTs go through the same auth/Origin rules as the rest of the UI.
    Saving a network while connected through the AP must show a page saying
    "the device is switching networks: reconnect your phone to <SSID> and
    open http://wa.local/". The WiFi card also takes an SSID and passphrase
    typed by hand (a hidden network never appears in a scan), and when the
    device has no WiFi adapter at all it says so instead of offering a scan
    that can only fail — same for the AP policy, which is pointless without
    an adapter to host it.
12. **Reset and status**: `!reset` and the web factory reset call
    `Manager.Erase()` (delete `wifi.json`) before restarting; `!status`
    lists each interface with its mode (`eth0 192.168.1.40`, `wlan0 AP
    WA-Setup-1A2B`, `wlan0 MyWifi 192.168.1.41`).

### Exit criteria

1. Unit tests for parsers, golden configs, the state-machine table and mDNS
   packets pass with `-race`.
2. `-network none` behaviour unchanged (Docker and dev machines).
3. `gofmt`, `go vet` clean; `make dist` still produces the three binaries.

### Gate out

M5 code is done → **M6 (Buildroot images) can start**. The on-device
validation is **M5b** and gates v1.0, not M6.

---

## M5b — Network validation on real hardware

Goal: prove that the network manager actually works on the boards, and pick
the WiFi dongle.

**Gate in**: M5 code done; a Raspberry Pi 2, a Cubieboard (Allwinner A10,
ARMv7, 1 GB) and a WiFi dongle. This milestone needs hardware and blocks
**v1.0** only.

**Why separate**: everything in M5 is verifiable only on a real network
stack. Keeping it apart lets M6 be developed and built without waiting for
the boards, and lets a failed dongle (no AP mode) be replaced without
blocking anything else.

### Steps

1. **Dongle gate** on both boards with **Raspberry Pi OS Lite** (Pi 2) and
   **Debian/Armbian** (Cubieboard): record `lsusb`,
   `iw list | grep -A 10 "Supported interface modes"` and the driver
   (`readlink /sys/class/net/wlan0/device/driver`). **Pass** if `AP` is
   listed and a 10-minute manual hostapd test lets a phone connect.
   **Fail** → buy an RT5370 or MT7612U dongle and repeat.
2. Run the binary in managed mode as a systemd service and execute the eight
   scenarios, three times each, on the Pi 2 and on the Cubieboard.
3. Record everything in [hardware.md](hardware.md). After M6 the same
   scenarios are repeated on the images (M6 exit criteria), so this milestone
   is "the code works on a normal distro".

### Exit criteria

All with the binary in managed mode as a systemd service; each scenario
repeated 3 times on the Raspberry Pi 2 + dongle (Raspberry Pi OS Lite) and
on the Cubieboard (Debian/Armbian, onboard ethernet + the same or a second
dongle):

1. Cold boot, no saved networks, no cable → `WA-Setup-XXXX` visible within
   60 s; a phone joins with the setup key; the captive page opens by itself
   (Android and iOS); `http://wa.local/` and `http://example.com/` both
   reach the UI.
2. Add the home WiFi in the UI → device joins within 60 s, AP disappears;
   from a laptop on the home WiFi `http://wa.local/` works; `!status` shows
   the new IP.
3. Wrong passphrase → back to the AP within 2 min, the UI shows the failure.
4. Router switched off → AP within 2 min (if unpaired or no cable);
   router back → device rejoins within 6 min when nobody is on the AP.
5. Ethernet to the router → `wa.local` reachable within 20 s. Ethernet
   directly to a laptop (no DHCP) → link-local address, `wa.local` reachable
   from the laptop within 40 s.
6. `!reset` → reboots into AP mode with no saved networks.
7. 24-hour soak with WiFi client mode: RSS growth < 10%, goroutine count
   stable (Status page), no reconnect loops in the logs.

### Gate out

M5b done on both boards → the network manager is trustworthy. Record the
dongles and the results in `docs/hardware.md`. This unlocks the runtime part
of M6's exit criteria and, together with M6, **v1.0**.

---

## M6 — Buildroot images

Goal: flashable SD-card images for the Raspberry Pi 2 and the BeagleBone
Black: boot in < 30 s to a working device, read-only root, data on its own
partition, survives power cuts.

**Gate in**: M5 steps done (the code is complete and unit-tested; its
hardware validation is M5b and does *not* block building images). The second
board is a **BeagleBone Black** (AM3358, ARMv7, 512 MB RAM, microSD boot).

**Status (2026-11-19): build side done.** `os/` holds the BR2_EXTERNAL tree
pinned to Buildroot 2024.02.9; `make -C os check` validates it and
`make -C os rpi2|bbb` produce `os/_output/<board>/images/sdcard.img`
(192 MB raw: 38.6 MB gzipped for the Pi 2, 47.8 MB for the BeagleBone
Black). Both images were inspected offline: boot partition contents, the
read-only root filesystem, the init scripts and the binary in
`/usr/bin/wa-transcriber`. Nothing has been booted yet; the runtime exit
criteria below need the boards.

### Steps

1. **Buildroot tree**: use the latest Buildroot **LTS** release; add it as a
   pinned download in `os/Makefile` (not a submodule). Create a BR2_EXTERNAL
   tree in `os/`: `external.desc` (`name: WAT`), `Config.in`, `external.mk`,
   `configs/`, `board/<name>/`, `package/`.
2. **Defconfigs**:
   - `os/configs/wat_rpi2_defconfig`, starting from Buildroot's
     `raspberrypi2_defconfig`;
   - BeagleBone Black: start from Buildroot's in-tree
     `beaglebone_defconfig` (kernel `omap2plus_defconfig`, U-Boot for
     `am335x_boneblack`, device tree `am335x-boneblack.dtb`). The board
     exposes a serial console over its USB client port; verify it early
     because it is the main way to debug boot.
   - Both: `BR2_TOOLCHAIN_BUILDROOT_MUSL`, busybox init, `/dev` via
     devtmpfs + mdev.
3. **Packages** (enable in both defconfigs): busybox (with `ip`, `udhcpc`,
   `ntpd`, `arping`, `watchdog`), `wpa_supplicant` (nl80211, no AP),
   `hostapd`, `dnsmasq`, `iw`, `wireless-regdb`, `ca-certificates`, the
   firmware for the chosen dongle (`linux-firmware` sub-option, e.g. Ralink
   rt2870 for RT5370, MediaTek MT76x2U), kernel drivers for that dongle as
   built-in or modules.
4. **The binary**: package `os/package/wa-transcriber` that copies the
   prebuilt `dist/wa-transcriber-linux-armv7` (built by `make build-arm7`
   from the same commit) to `/usr/bin/`. Do not build Go inside Buildroot
   (its Go version lags; our builds are static already).
5. **Partitions** (`board/*/genimage.cfg`): (1) FAT boot with firmware,
   kernel, DTB and `wat.txt`; (2) root, ext4, mounted read-only;
   (3) data, ext4 label `DATA`, created at first boot.
   - `wat.txt` template: `setup_ap_key=`, `country=IT`, `hostname=wa`,
     optional `wifi_ssid=`/`wifi_passphrase=` (imported into
     `/data/wifi.json` when the network manager starts, so a card can be
     prepared with the home network and joins on its own first boot; the
     import is idempotent and never promotes the network over one added
     later in the UI), with comments explaining each line.
   - first-boot script `/etc/init.d/S01data`: if no partition labelled
     `DATA` exists, create it with `sfdisk --append` in the free space, run
     `mkfs.ext4 -L DATA`, then mount `/data` with `noatime`. Must be
     idempotent (a power cut during first boot must not brick the card).
6. **Read-only root**: `BR2_TARGET_GENERIC_REMOUNT_ROOTFS_RW` off; tmpfs
   for `/tmp`, `/run`, `/var/log`, `/var/lib/misc` (dnsmasq leases) in
   `/etc/fstab`.
7. **Services** (`/etc/inittab` + init scripts):
   - `ntpd -n -p pool.ntp.org` (respawn);
   - hardware watchdog: `bcm2835_wdt` / `omap_wdt` + busybox `watchdog -T
     15 /dev/watchdog`;
   - `::respawn:/usr/bin/wa-transcriber -data /data -network managed
     -listen :80` (respawn covers crashes; the binary re-execs itself for
     reset).
8. **Build automation**: `make -C os rpi2` / `make -C os beagle` produce
   `os/output/<board>/images/sdcard.img`; a release workflow job builds both
   on tags with the Buildroot `dl/` directory and ccache cached (a cold
   build takes about an hour).
9. **Docs**: README "Flashing" section (Raspberry Pi Imager / `dd`, how to
   edit `wat.txt`, first boot walkthrough); upgrades in v1 = reflash the
   image; `/data` is lost unless the data partition is kept — document how
   to back up `/data` (web UI download is a possible later item).

### Exit criteria

Build side (no hardware needed, so these are checked first):

- `make -C os check` passes: the BR2_EXTERNAL tree is well formed
  (external.desc, Config.in, package metadata, genimage configs, init
  scripts, `wat.txt` template).
- Both images build from a clean checkout with one command each
  (`make -C os rpi2`, `make -C os bbb`).
- Compressed image < 64 MB; read-only root, `DATA` partition created at
  first boot.

Runtime side (needs the boards; may be done together with M5b):

- Boot to web UI reachable < 30 s on the Pi 2 (< 45 s on the BeagleBone
  Black).
- Idle RAM: wa-transcriber RSS < 60 MB; whole system < 100 MB used on the
  Pi 2 and the 512 MB BeagleBone Black with ≥ 30 MB free.
- All M5b scenarios pass on **both** boards running the images.
- Power-cut test: 50 random power cuts (including during pairing and while
  saving settings) → the device always boots, `config.json` and
  `session.db` stay valid (no re-pairing needed).
- Watchdog test: `kill -STOP` the binary → device reboots within 60 s and
  comes back.

### Gate out

Tag **v1.0**. Images attached to the release.

---

## M9 — Binary updates from GitHub releases (boards only)

**Gate in**: M7 releases exist (the `manifest.json` asset is added there) and
M6 images build. Code and tests need no hardware; only the field test does.

**Decision**: D17 in [ARCHITECTURE.md](ARCHITECTURE.md). The unit of update
is the **binary**. The OS image stays a reflash (it needs a second root slot
and `swupdate`/RAUC, which changes the partition table); Docker is out, the
scratch image has nothing to exec from a volume.

### Steps

1. **`internal/update`** — manifest parsing and its ed25519 signature,
   version compare, arch/`EM_*` check, atomic download + install, the
   three-slot state machine (promote / rollback / prune), update check with a
   cache. Unit tests against `httptest` and temp dirs. No wiring yet.
2. **Release side** — `manifest.json` generated in `dist/` on tags,
   `tools/wat-keygen` + `tools/wat-sign`, `tools/wat-manifest`, the public key
   committed as `release/update-pubkey.txt` and injected with
   `-X main.updatePubKey` (empty = self-update compiled out), a
   `make sign-manifest` target. The owner generates the keypair once and keeps
   the private key offline.
3. **Bootstrap in `main`** — `-update-dir` (env `WAT_UPDATE_DIR`, empty =
   off), run before anything else; `restart()` execs the bootstrap path instead
   of `os.Executable()`; `verified` after 60 s; `/data/rollback` snapshot
   before the first exec of a new version. `inittab` passes
   `-update-dir /data/bin`; `make -C os check` covers it.
4. **Config and UI** — `update_check` (`off`|`daily`, default off: the opt-in
   daily check) and `update_auto` (install and restart without asking, still
   behind the health gate and never while a note is queued or transcribing).
   System page card: current version, "check now", "v0.3.0 available" + notes
   link, "install and restart", "back to v0.1.1". Strings in en/it.
5. **Field test on the boards** (owner) — see the exit criteria.

### Exit criteria

Code (no hardware):

- `go test -race ./...` covers promote, rollback, prune, a tampered binary, a
  wrong-arch asset, an unsigned or badly signed manifest, a truncated
  download, and a state file that points at a missing file; gofmt/vet clean;
  CI green.
- With `-update-dir` empty (Docker, dev machines) the feature is inert: no
  network call, no card in the UI.

On both boards (owner):

- Update over WiFi from the current release to the next one: the device
  restarts itself, keeps its session (no re-pairing), `!status` and the System
  page show the new version.
- `kill -9` during the download and during the install → the device keeps
  running the old version, no `.part` survives the next boot, nothing broken.
- Power cut during the install → the device boots and runs a working version
  (old or new).
- A deliberately broken binary (same version numbers, `return 1` in `main`) →
  rolled back automatically after three tries, `last_failure` visible in the
  UI, device still transcribes.
- Manual rollback to the previous version works, and restores
  `config.json`/`session.db`.
- Update right after a reflash (factory binary newer than `/data`'s active
  slot) → the newer factory binary wins.
- At most three binaries in `/data/bin` at any time.

### Gate out

Ship as **v1.1**. Full-image updates are **M10**, install to internal flash
is **M11**; both need a partition-table change, so they cost every existing
device one last reflash.

---

## M10 — Image (OS) updates in place

**Gate in**: M9 shipped (v1.1): devices already update the binary, the signed
`manifest.json` exists and the owner holds the key. Hardware: a BeagleBone
Black and a Pi 2 running a v1.1 image.

**Decision**: the OS is updated by writing a **second root slot**, never over
the running one, and `/data` is never touched by an image update.

Layout today: p1 boot (FAT), p2 root (ext4, read-only), p3 data (ext4, made on
first boot). Layout for M10: p1 boot, **p2 root A, p3 root B** (both ext4,
read-only), p4 data. Moving from one root to two changes the partition table
and the root's identity, so it costs exactly one reflash; after that every
image update is in place.

- The choice of slot lives in the **boot partition** (both bootloaders read
  FAT): `uEnv.txt` on the BeagleBone Black, `config.txt` + `cmdline.txt` on
  the Pi 2. An update writes the inactive slot, verifies it, then flips one
  file and reboots.
- **Health gate, same shape as M9**: starting a slot that has not confirmed
  itself bumps a counter; the running system confirms after 60 s (reuse the
  existing timer) and clears it; three unconfirmed starts fall back to the
  other slot. What is implemented counts starts **in userspace** and rewrites
  the boot file back, so it covers an image that boots but whose app never
  becomes healthy — the realistic failure of an image update. A kernel or an
  init that never reaches userspace needs the U-Boot `bootcount`
  (`BOOTCOUNT_LIMIT`, `ENV_IS_IN_FAT` and `bootlimit` are already in the
  image's U-Boot, `bootlimit` is 0), which can only be proven by flashing and
  is tracked as its own step (wat-kfd.6).
- **Manifest**: `manifest.json` gains `images: {beagle: {...}, rpi2: {...}}`
  with file, sha256, size and the **layout version** it requires. Same key,
  same signature. A device still on the old table prints "reflash once to
  enable image updates" instead of downloading 200 MB.

### Steps

1. **Partitions**: genimage configs, `mkimage.sh`, `S01data` (finds DATA by
   label already), `os/check.sh`; both images still build.
2. **Bootloaders** (the real work, needs the consoles): slot selection and
   the boot counter in `uEnv.txt` on the BBB; the Pi 2 equivalent. Prove each
   by hand before writing Go.
3. **`internal/imageupdate`**: download to `/data`, stream into the inactive
   slot while hashing, fsync, verify by read-back, then flip the boot file and
   the counter; never touch the running slot; reuse the M9 signature, atomic
   write and health-timer code.
4. **UI**: a "System image" card (slot in use, available image, install and
   reboot, back to the previous image), hidden on the old layout where it
   shows the "reflash once" line. `update_auto` covers images too, off by
   default and never while a note is queued.
5. **Release**: the workflow attaches both board images, `wat-manifest` writes
   the `images` block, README/ARCHITECTURE updated.
6. **U-Boot bootcounter** (needs the console, after the first flash with the
   two-slot layout): `bootlimit=3` + `altbootcmd` in the environment, the
   updater setting `upgrade_available` and clearing it from the 60 s health
   timer with `fw_setenv` (uboot-tools), so a kernel that never reaches
   userspace also falls back. Verify by flashing a slot with a kernel that
   panics, not by reading the manual.

### Exit criteria

- Both boards: an image update over the network → device boots the new root,
  `/data` untouched (still paired), the UI shows the new image, the old slot
  is still bootable and can be selected back.
- Power cut 10× at random points (during the write, after the flip, before
  the confirmation) → always comes up on a working slot.
- An image whose app never becomes healthy (init starts but the binary exits)
  → the userspace gate rewrites the boot file back and reboots into the old
  slot after three unconfirmed starts. A kernel that never mounts the root
  needs step 6 and its own power-cut test.
- An old-layout device refuses with one line, never downloads.
- Free space checked against the image size before the download starts.

### Gate out

**v1.2**. After this the only reason to touch a card is a damaged bootloader.

---

## M11 — Install from the SD card to the internal flash

Why: the BeagleBone Black has a 4 GB eMMC and SD cards die. A device running
from eMMC has no card to lose, and M9/M10 work there unchanged because the
layout is identical.

**Gate in**: M10 (two root slots) — otherwise the install repeats the
partition work. Hardware: a BBB and one SD card.

### Steps

1. **Target selection by rule, not guess**: the block device whose
   `/sys/block/*/device/type` is `MMC`, that is not the device the running
   root came from, with its size; refuse when ambiguous and print the
   candidates.
2. **Clone mode**: `wa-transcriber -flash /dev/mmcblk1` and a System page
   button behind a typed confirmation (like `RESET`). It writes *this card's*
   image to the eMMC: MLO/U-Boot, boot partition, root slot, and a fresh DATA
   partition — read from the SD card, nothing downloaded, so it works
   offline.
3. **`/data` migration**: during the install, stage `config.json`,
   `session.db*` and `wifi.json` from the running `/data` into
   `wat-migrate.d/` on the target's boot partition (free space checked first;
   if they do not fit the clone still completes and says the device will boot
   unpaired), then write `wat-migrate` last. On the first boot from the eMMC,
   `S01data` creates DATA and copies the staged files into it with temp name,
   fsync and rename. *Not* by naming the card's DATA partition: the first eMMC
   boot is the one after the card was removed, so that device is never there
   (the old `from` line is still read, but nothing depends on it).
4. **Verify before declaring success**: read the written regions back and
   compare against the card before saying "remove the SD and reboot". On a
   mismatch the eMMC is simply not bootable yet and the card still is — that
   is the recovery path, and the operation is repeatable.
5. **Progress on the console** (the UI will not survive the reboot) and in
   the log buffer; a power cut leaves the eMMC half-written and the card
   intact.
6. **Boot order**: the AM335x ROM tries the SD first; document and check that
   removing the card is what boots the eMMC, and that the eMMC carries its own
   MLO/U-Boot at the expected offsets.
7. **Docs**: README "install to internal flash", ARCHITECTURE note.

### Exit criteria

- On the BBB: install from SD to eMMC, power off, remove the card, boot from
  eMMC → paired, same settings, transcribes, `/data` intact.
- An M10 image update works on the eMMC device exactly as on the card.
- Power cut during the install → the card still boots the device, re-running
  the install completes it.
- Refuses when the target is ambiguous, and refuses on a board without
  internal flash (Pi 2) with one line.

### Gate out

**v1.3**, or folded into v1.2 if both land together.

---

## Appendix — test checklist template

Copy into the PR description for any field test:

```
Device / OS:            App versions:
Binary version (git describe):
Scenario:
Steps performed:
Expected:
Observed:
Logs (Logs page → Download) attached: yes/no
Result: PASS / FAIL
```
