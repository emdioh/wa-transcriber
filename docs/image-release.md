# Publishing a release with board images

How to publish a GitHub release that a device can update itself from, including
the OS images for the **BeagleBone Black** and the **Raspberry Pi 2**.
`docs/ARCHITECTURE.md` §11 explains the mechanisms (D17 binary updates, D18 two
root slots, M11 install); this file is the procedure, in the order that works.

The Makefile has `make sign-manifest` for the binary-only case; everything else
here was learned the hard way and is not in one command.

## What the device does with what you publish

1. It GETs `<manifest URL>` (default
   `https://github.com/emdioh/wa-transcriber/releases/latest/download/manifest.json`,
   overridable with `WAT_UPDATE_URL`) and `<manifest URL>.ed25519`, and verifies
   the detached ed25519 signature against the public key **compiled into the
   binary** (`release/update-pubkey.txt`, via `-X main.updatePubKey`).
2. It compares `version` with its own: equal versions are **not** an upgrade, so
   a device already on `v0.2.0` is never offered `v0.2.0` again.
3. If `images.<board>` exists it prefers the image over the binary: one
   download, one reboot, and the image brings its own binary. The image URL is
   the **manifest URL with its last path element replaced** by
   `images.<board>.file`.
4. It verifies the download's `sha256` and `size`, writes the image into the
   *inactive* root slot, reads it back, switches the boot file and reboots. A
   new slot that does not come up three times is abandoned for the old one.
5. `<board>` comes from the device's `/etc/wat/image.json` (`beagle` or
   `rpi2`), and the manifest entry must carry the layout the device can take
   (`layout: 2` today: `p1` boot, `p2` root A, `p3` root B, `p4` DATA).

Consequence: **a private repository cannot be used for this.** Its release
assets answer 404 to an unauthenticated client, which is what the device is.
If release URLs stop working, check visibility first.

## 0. Prerequisites

- A clean tree on the commit to release (`git status --short` empty), and
  `gh` authenticated as `emdioh` (`gh auth status`, needs `repo`).
- The signing key file, by default `$HOME/wat-update.key`, generated once with
  `go run ./tools/wat-keygen -out "$HOME/wat-update.key"`. Its **public** half
  must be the base64 line in `release/update-pubkey.txt`, or the device will
  download the manifest and then reject the signature. If you regenerate the
  keypair, every device in the field needs a new image (the key is compiled in).
- Space: each board image is ~369 MB raw; the release holds one asset per
  board.

## 1. Tag, and let CI build the binaries

```sh
cd ~/dev/wa-transcriber
git tag -a v0.2.0 -m "v0.2.0: what changed"
git push origin v0.2.0
```

The `Release` workflow (`on: push, tags: [v*]`) builds
`dist/wa-transcriber-linux-{armv7,arm64,amd64}`, `SHA256SUMS` and an
**unsigned** `manifest.json` (no `images` block), and creates the release.
Wait for it: `gh run list --limit 3`, or `gh run watch`.

Do not upload hand-built binaries: the manifest hashes must match the assets
that are actually published, or a device that falls back to the binary path
rejects the download.

## 2. Build the board images *from the tag*

```sh
make -C os bbb      # -> os/_output/bbb/images/sdcard.img
make -C os rpi2     # -> os/_output/rpi2/images/sdcard.img
```

Build them **after** tagging: the version compiled into the binary comes from
`git describe --tags --always --dirty`. An image built before the tag reports
the previous version, and every device flashed with it would keep being offered
the same release. A cold build (no `os/_output/<board>`) also needs the
downloads in `os/_buildroot/buildroot-2024.02.9/dl`; the Debian-listed
`linux-firmware` tarball alone is 364 MB.

Check the version inside the image before publishing (the file name tells you
nothing):

```sh
for b in bbb rpi2; do
  debugfs -R "dump /usr/bin/wa-transcriber /tmp/imgbin-$b" \
          os/_output/$b/images/rootfs.ext2 2>/dev/null
  printf '%-5s ' "$b"
  strings /tmp/imgbin-$b | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | sort -u | tr '\n' ' '
  echo
done
```

Expect exactly the tag, e.g. `v0.2.0`.

## 3. Stage the images under the names the manifest expects

`tools/wat-manifest` has a fixed table — `beagle.img` and `rpi2.img` — and
skips a file that does not exist (a missing board is simply absent from the
manifest, not an error). Copy, never rename in place:

```sh
mkdir -p /tmp/rel/dist /tmp/rel/images

# the published binaries, so the manifest's asset hashes are the release's
gh release download v0.2.0 --repo emdioh/wa-transcriber \
    --pattern 'wa-transcriber-linux-*' --dir /tmp/rel/dist --clobber

cp os/_output/bbb/images/sdcard.img  /tmp/rel/images/beagle.img
cp os/_output/rpi2/images/sdcard.img /tmp/rel/images/rpi2.img
```

## 4. Generate the manifest with the images block

```sh
go run ./tools/wat-manifest \
    -dist /tmp/rel/dist -images /tmp/rel/images \
    -version v0.2.0 -out /tmp/rel/manifest.json
```

The output is deterministic for identical inputs (fixed key order, indent 2),
which is what makes a signature reproduceable — except `released_at`, which is
"now". Optional: `-min-from v0.1.1` (oldest version allowed to jump straight to
this one), `-notes-url <url>` (default: the GitHub tag page),
`-out <path>`.

Diff it against CI's version to be sure you only added what you meant to:

```sh
gh release download v0.2.0 --pattern manifest.json --dir /tmp/rel/ci --clobber
diff /tmp/rel/ci/manifest.json /tmp/rel/manifest.json
```

Expect `released_at` plus the `images` block, nothing else.

## 5. Upload the images and the manifest together

```sh
gh release upload v0.2.0 --repo emdioh/wa-transcriber --clobber \
    /tmp/rel/images/beagle.img /tmp/rel/images/rpi2.img /tmp/rel/manifest.json
```

This **replaces** CI's image-less manifest. From here on the release is
untrusted for devices until step 6: the old signature no longer matches.

## 6. Sign it

```sh
make sign-manifest VERSION=v0.2.0 KEY=$HOME/wat-update.key
```

It downloads `manifest.json` from the release, signs those exact bytes with
`tools/wat-sign`, and uploads `manifest.json.ed25519`. Nothing else to do.

## 7. Verify before announcing it

```sh
cd ~/dev/wa-transcriber
base=https://github.com/emdioh/wa-transcriber/releases/latest/download

curl -sL -o /tmp/live.json "$base/manifest.json"
curl -sL -o /tmp/live.sig  "$base/manifest.json.ed25519"
python3 -m json.tool /tmp/live.json | head -12          # version + images keys
curl -sIL -o /dev/null -w 'image: %{http_code}\n' \
    "https://github.com/emdioh/wa-transcriber/releases/download/v0.2.0/rpi2.img"
```

Then check that what the release serves is what you hashed, and that the
signature matches those bytes. There is no `-verify` in `tools/wat-sign`;
this is the check, using the same key the device trusts:

```sh
mkdir -p /tmp/verifykey && cat > /tmp/verifykey/main.go <<'EOF'
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func firstKey(path string) string {
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return ""
}

func main() {
	pub, err := base64.StdEncoding.DecodeString(firstKey(os.Args[1]))
	if err != nil {
		panic(err)
	}
	msg, _ := os.ReadFile(os.Args[2])
	sig, err := base64.StdEncoding.DecodeString(firstKey(os.Args[3]))
	if err != nil {
		panic(err)
	}
	fmt.Println("signature valid:", ed25519.Verify(pub, msg, sig))
}
EOF
cd /tmp/verifykey && go mod init verifykey >/dev/null 2>&1
go run . ~/dev/wa-transcriber/release/update-pubkey.txt /tmp/live.json /tmp/live.sig
```

And that the manifest's hashes are the published files:

```sh
python3 - <<'EOF'
import json, hashlib
m = json.load(open('/tmp/live.json'))
for name, e in {**m.get('assets', {}), **m.get('images', {})}.items():
    print(name, e['file'], e['size'], e['sha256'][:16] + '…')
EOF
```

`images.<board>.sha256` must equal `sha256sum os/_output/<board>/images/sdcard.img`.

## Rules that bite

- **Sign last, and never touch `manifest.json` afterwards.** Re-uploading it
  (a re-run of step 4, a corrected image) invalidates the signature; the device
  refuses a manifest that does not verify, with a generic failure in the log.
- Re-running step 4 changes `released_at` and therefore the bytes: always
  re-do step 6.
- The `images` block is pinned to `layoutVersion = 2`. An image with another
  partition layout is not offerable this way; changing the layout means a
  reflash for every device in the field.
- Equal version ⇒ no update. To re-issue the same version, bump it.
- A release that turns out wrong is retired by publishing a **higher** version;
  deleting a tag/release leaves devices with whatever they downloaded, and a
  device that already switched slots keeps running it.
- `beagle.img` and `rpi2.img` are the *same layout* but not the same bytes; the
  manifest keys them by board, not by architecture (both root slots hold the
  armv7 binary).

## Adding a board image to an existing release

Exactly steps 2–7 again, with the full set of images in `/tmp/rel/images`:

- the manifest is regenerated from the published binaries and re-signed;
- entries for files that did not change keep byte-identical hashes, so devices
  that already updated are unaffected;
- devices on that board that have not updated yet simply start seeing the image.

This is how `rpi2.img` was added to `v0.2.0` after `beagle.img` was already
published and signed.
