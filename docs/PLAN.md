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
     │
     ├──► M2 experiment ──► M3b coordination
     │
     └──► M5 network manager ──► M6 Buildroot images ──► v1.0
```

M7 first: it needs no hardware and protects every later change. M2/M3b and
M5/M6 are independent tracks; do them in whatever order the hardware and the
test phones become available.

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
variant or prefix).

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

## M5 — Network manager

Goal: the device is always reachable: ethernet with DHCP or link-local,
saved WiFi networks, or its own setup access point; `wa.local` resolves on
every network; WiFi is configured from the web UI.

**Gate in** (hardware gate — do this first, it decides whether to buy a
dongle):

1. On the Raspberry Pi 2 with **Raspberry Pi OS Lite** (fine for M5
   development; M6 replaces it) and on the **Cubieboard** (Allwinner A10,
   ARMv7, 1 GB; Debian/Armbian image), plug the WiFi dongle and record:
   `lsusb`, `iw list | grep -A 10 "Supported interface modes"`, the kernel
   driver (`readlink /sys/class/net/wlan0/device/driver`).
2. **Pass** if `AP` is listed and a 10-minute manual hostapd test (step 5's
   config by hand) lets a phone connect. **Fail** → buy an RT5370 or
   MT7612U dongle and repeat.
3. Record the result in `docs/hardware.md`.

Development tip: most of M5 can be tested on any Linux laptop with simulated
WiFi radios: `sudo modprobe mac80211_hwsim radios=2` creates `wlan0` and
`wlan1` that can talk to each other (one as AP, one as client).

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
     -s /usr/share/udhcpc/default.script` (Buildroot/busybox provides the
     script; on Raspberry Pi OS install `udhcpc`);
   - no lease within 15 s → link-local: address `169.254.(h>>8 % 254 +1).(h % 254 + 1)`
     from a hash of the MAC; check with `arping -D -c 2 -I <if> <ip>`; on
     conflict hash again (max 10 tries); `ip addr add <ip>/16 dev <if>`;
     keep retrying DHCP every 60 s and drop the link-local address when a
     lease arrives;
   - carrier down → flush addresses (`ip addr flush dev <if>`).
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
     `dns-sd -G v4 wa.local` (macOS), `ping wa.local` (Windows 10+), Android
     and iOS browsers.
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
    open http://wa.local/".
12. **Reset and status**: `!reset` and the web factory reset call
    `Manager.Erase()` (delete `wifi.json`) before restarting; `!status`
    lists each interface with its mode (`eth0 192.168.1.40`, `wlan0 AP
    WA-Setup-1A2B`, `wlan0 MyWifi 192.168.1.41`).

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
8. Unit tests for parsers, golden configs, state machine table, mDNS
   packets all pass; `-network none` behaviour unchanged (Docker).

### Gate out

M5 done on the Pi 2 (Raspberry Pi OS) and the Cubieboard (Debian/Armbian).
The BeagleBone Black is validated in M6. Record the dongles and the results
in `docs/hardware.md`.

---

## M6 — Buildroot images

Goal: flashable SD-card images for the Raspberry Pi 2 and the BeagleBone
Black: boot in < 30 s to a working device, read-only root, data on its own
partition, survives power cuts.

**Gate in**: M5 exit criteria met. The second board is a **BeagleBone
Black** (AM3358, ARMv7, 512 MB RAM, microSD boot).

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
     with comments explaining each line.
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

- Both images build from a clean checkout with one command each.
- Compressed image < 64 MB; boot to web UI reachable < 30 s on the Pi 2
  (< 45 s on the BeagleBone Black).
- Idle RAM: wa-transcriber RSS < 60 MB; whole system < 100 MB used on the
  Pi 2 and the 512 MB BeagleBone Black with ≥ 30 MB free.
- All M5 scenarios pass on **both** boards running the images.
- Power-cut test: 50 random power cuts (including during pairing and while
  saving settings) → the device always boots, `config.json` and
  `session.db` stay valid (no re-pairing needed).
- Watchdog test: `kill -STOP` the binary → device reboots within 60 s and
  comes back.

### Gate out

Tag **v1.0**. Images attached to the release.

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
