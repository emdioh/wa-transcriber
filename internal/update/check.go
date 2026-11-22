package update

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultTTL is how long a successful check is trusted.
const DefaultTTL = time.Hour

// manifestLimit caps the manifest body (generous: a few KB in practice).
const manifestLimit = 1 << 20

// Checker looks up the latest release. It GETs ManifestURL (manifest.json)
// and ManifestURL+".ed25519" (the detached signature), verifies the signature
// with Pub, parses the manifest and caches the result in memory and, when
// CachePath is set, on disk as JSON. A failed check never overwrites a good
// cache: the UI can then still show "last known release" and its age.
type Checker struct {
	ManifestURL string
	Client      *http.Client
	Pub         ed25519.PublicKey
	CachePath   string
	TTL         time.Duration
	Now         func() time.Time

	mu    sync.Mutex
	cache cachedManifest
}

// cachedManifest is the on-disk cache entry: the exact bytes served as
// manifest.json and its detached signature, so the on-disk copy is checked
// against the signature like a fresh one (a tampered cache file is refused,
// not trusted). Both are base64: a json.RawMessage would be re-indented by
// the encoder, and the signature is over the bytes, whitespace included.
type cachedManifest struct {
	FetchedAt time.Time `json:"fetched_at"`
	Manifest  string    `json:"manifest_b64"`
	Signature string    `json:"signature_b64"`
}

// manifestBytes returns the manifest as it was served.
func (e *cachedManifest) manifestBytes() []byte {
	b, err := base64.StdEncoding.DecodeString(e.Manifest)
	if err != nil {
		return nil
	}
	return b
}

// sigBytes returns the decoded detached signature.
func (e *cachedManifest) sigBytes() ([]byte, error) {
	return base64.StdEncoding.DecodeString(e.Signature)
}

// valid reports whether the entry carries anything at all.
func (e *cachedManifest) valid() bool { return e.Manifest != "" && e.Signature != "" }

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

func (c *Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Latest returns the newest release manifest. While the cache is younger
// than TTL no request is made. With no key compiled in (Pub == nil) it fails
// fast: an unverifiable manifest must never be offered.
func (c *Checker) Latest(ctx context.Context) (*Manifest, error) {
	return c.latest(ctx, false)
}

// Refresh is Latest that ignores the cache: the button that says "check now"
// must reach GitHub even when a check ran ten minutes ago (that is how a
// release published within the TTL is ever seen).
func (c *Checker) Refresh(ctx context.Context) (*Manifest, error) {
	return c.latest(ctx, true)
}

func (c *Checker) latest(ctx context.Context, force bool) (*Manifest, error) {
	if len(c.Pub) != ed25519.PublicKeySize {
		return nil, errors.New("update: no update public key, updates are off")
	}
	if c.ManifestURL == "" {
		return nil, errors.New("update: no manifest URL")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		if m, ok := c.fresh(); ok {
			return m, nil
		}
	}
	m, entry, err := c.check(ctx)
	if err != nil {
		// Leave memory and disk cache as they are; report the error.
		return nil, err
	}
	c.cache = entry
	c.writeCache(entry)
	return m, nil
}

// Cached returns the last known verified manifest without any network access,
// whatever its age, or nil when there is none. It is what the UI shows when a
// check cannot run (no network): "v0.3.0 was available an hour ago". The TTL
// only decides whether Latest makes a request; callers that need to know the
// age can read the cache entry themselves.
func (c *Checker) Cached() *Manifest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known()
}

// known returns the cached manifest after loading the disk cache on first
// use, ignoring the TTL. The caller holds c.mu.
func (c *Checker) known() *Manifest {
	if !c.cache.valid() && c.CachePath != "" {
		if e, ok := c.readCache(); ok {
			c.cache = e
		}
	}
	if !c.cache.valid() {
		return nil
	}
	m, err := ParseManifest(c.cache.manifestBytes())
	if err != nil {
		return nil
	}
	return m
}

// fresh returns the cached manifest when it is still within TTL: Latest then
// skips the network.
func (c *Checker) fresh() (*Manifest, bool) {
	m := c.known()
	if m == nil || c.now().Sub(c.cache.FetchedAt) > c.ttl() {
		return nil, false
	}
	return m, true
}

// check performs the two GETs and verifies them.
func (c *Checker) check(ctx context.Context) (*Manifest, cachedManifest, error) {
	body, err := c.get(ctx, c.ManifestURL)
	if err != nil {
		return nil, cachedManifest{}, fmt.Errorf("update: manifest: %w", err)
	}
	sigB64, err := c.get(ctx, c.ManifestURL+".ed25519")
	if err != nil {
		return nil, cachedManifest{}, fmt.Errorf("update: signature: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(trimAll(string(sigB64)))
	if err != nil {
		// Some tools write the signature as raw (unpadded) base64.
		sig, err = base64.RawStdEncoding.DecodeString(trimAll(string(sigB64)))
		if err != nil {
			return nil, cachedManifest{}, fmt.Errorf("update: decode signature: %w", err)
		}
	}
	if err := VerifyManifest(c.Pub, body, sig); err != nil {
		return nil, cachedManifest{}, err
	}
	m, err := ParseManifest(body)
	if err != nil {
		return nil, cachedManifest{}, err
	}
	return m, cachedManifest{
		FetchedAt: c.now(),
		Manifest:  base64.StdEncoding.EncodeToString(body),
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// get fetches one URL with a size cap.
func (c *Checker) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, manifestLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > manifestLimit {
		return nil, errors.New("response too large")
	}
	return b, nil
}

// readCache loads and verifies the on-disk cache. A cache entry that does not
// verify is ignored.
func (c *Checker) readCache() (cachedManifest, bool) {
	b, err := os.ReadFile(c.CachePath)
	if err != nil {
		return cachedManifest{}, false
	}
	var e cachedManifest
	if err := json.Unmarshal(b, &e); err != nil || !e.valid() {
		return cachedManifest{}, false
	}
	sig, err := e.sigBytes()
	if err != nil {
		return cachedManifest{}, false
	}
	body := e.manifestBytes()
	if body == nil {
		return cachedManifest{}, false
	}
	if err := VerifyManifest(c.Pub, body, sig); err != nil {
		return cachedManifest{}, false
	}
	if _, err := ParseManifest(body); err != nil {
		return cachedManifest{}, false
	}
	return e, true
}

// writeCache stores the entry, best effort: a cache that cannot be written is
// a slower next start, not a failure.
func (c *Checker) writeCache(e cachedManifest) {
	if c.CachePath == "" {
		return
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(c.CachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".update-check-*.json")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmp.Name(), c.CachePath); err != nil {
		return
	}
	syncDir(dir)
}

// trimAll drops every space, tab, CR and LF (the signature file is one
// base64 line, sometimes with a trailing newline).
func trimAll(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
