package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// realBinary returns the bytes of the running test binary: a genuine
// executable for this machine, which is what CheckExecutable insists on.
func realBinary(t *testing.T) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// manifestFor builds a one-asset manifest for body under the key given.
func manifestFor(t *testing.T, version, key string, body []byte) *Manifest {
	t.Helper()
	m := &Manifest{
		Schema:     1,
		Version:    version,
		ReleasedAt: testNow,
		Assets: map[string]Asset{
			key: {File: "wa-transcriber-" + version + "-" + key, SHA256: Sum(body), Size: int64(len(body))},
		},
	}
	return m
}

func TestFetch(t *testing.T) {
	body := realBinary(t)
	own := ownKey(t)
	foreign := otherKey(t)
	small := []byte("not really a binary")

	tests := []struct {
		name string
		// respond writes the handler's answer; size/shacheck select the
		// manifest that is handed to Fetch.
		respond func(w http.ResponseWriter, r *http.Request)
		// assetBody is what the manifest describes; servedBody is what the
		// handler returns (defaults to assetBody when nil).
		assetBody  []byte
		assetKey   string
		assetSize  int64 // 0 = len(assetBody)
		assetSHA   string
		wantErr    bool
		checkFinal bool // expect the file installed and pending
	}{
		{
			name:       "happy path",
			assetBody:  body,
			assetKey:   own,
			checkFinal: true,
		},
		{
			name:      "wrong sha256",
			assetBody: body,
			assetKey:  own,
			assetSHA:  goodSHA,
			wantErr:   true,
		},
		{
			name:       "truncated body",
			assetBody:  body,
			assetKey:   own,
			assetSize:  int64(len(body)) + 4096,
			checkFinal: false,
			wantErr:    true,
		},
		{
			name:      "oversized body",
			assetBody: body,
			assetKey:  own,
			// The manifest says 16 bytes; the server sends the whole binary.
			assetSize: 16,
			assetSHA:  Sum(body[:16]),
			wantErr:   true,
		},
		{
			name:      "arch mismatch",
			assetBody: body,
			assetKey:  foreign, // the manifest offers it, but it is this machine's binary
			wantErr:   true,
		},
		{
			name:      "no asset for this platform",
			assetBody: body,
			assetKey:  "", // Asset() will miss
			wantErr:   true,
		},
		{
			name:      "text file instead of a binary",
			assetBody: small,
			assetKey:  own,
			wantErr:   true,
		},
		{
			name: "http 500",
			respond: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			assetBody: body,
			assetKey:  own,
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assetBody := tc.assetBody
			respond := tc.respond
			if respond == nil {
				respond = func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write(assetBody)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(respond))
			defer srv.Close()

			dataDir, binDir, updDir := binDirs(t)
			_ = dataDir
			// A pending version from an earlier attempt that must survive the
			// failure untouched.
			oldPending, oldPendingSHA := fakeBinary(t, binDir, "v0.1.5", "previous pending", 0o755)
			mustSave(t, binDir, &State{
				Schema:  1,
				Pending: &Slot{Version: "v0.1.5", File: oldPending, SHA256: oldPendingSHA},
			})

			key := own
			if tc.assetKey != "" {
				key = own
			}
			m := manifestFor(t, "v0.2.0", key, assetBody)
			if tc.assetSize != 0 {
				a := m.Assets[key]
				a.Size = tc.assetSize
				m.Assets[key] = a
			}
			if tc.assetSHA != "" {
				a := m.Assets[key]
				a.SHA256 = tc.assetSHA
				m.Assets[key] = a
			}
			fetchKey := tc.assetKey
			if fetchKey == "" {
				fetchKey = "linux-pdp11"
			}

			var reqs atomic.Int64
			inst := &Installer{
				Dir: binDir,
				Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					reqs.Add(1)
					return http.DefaultTransport.RoundTrip(r)
				})},
				Now: func() time.Time { return testNow },
			}
			slot, err := inst.Fetch(context.Background(), srv.URL+"/bin", m, fetchKey)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Fetch succeeded: %+v", slot)
				}
				if reqs.Load() > 0 && tc.assetKey != "" && tc.assetKey != own {
					// arch mismatch is detected after the download
				}
				st := mustLoad(t, binDir)
				if st.Pending == nil || st.Pending.Version != "v0.1.5" || st.Pending.SHA256 != oldPendingSHA {
					t.Fatalf("failed Fetch changed Pending: %+v", st.Pending)
				}
				if !exists(oldPending) {
					t.Fatal("failed Fetch deleted the old pending file")
				}
				if exists(BinaryName(binDir, "v0.2.0")) {
					t.Fatal("failed Fetch left the new binary in place")
				}
				parts, _ := filepath.Glob(filepath.Join(updDir, "*.part"))
				if len(parts) != 0 {
					t.Fatalf("failed Fetch left %v behind", parts)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			want := BinaryName(binDir, "v0.2.0")
			if slot.Version != "v0.2.0" || slot.File != want || slot.SHA256 != Sum(assetBody) {
				t.Fatalf("slot = %+v", slot)
			}
			if !exists(want) {
				t.Fatal("binary not installed")
			}
			fi, err := os.Stat(want)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o111 == 0 {
				t.Fatalf("mode %v is not executable", fi.Mode().Perm())
			}
			got, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(assetBody) {
				t.Fatal("installed bytes differ from the served body")
			}
			st := mustLoad(t, binDir)
			if st.Pending == nil || st.Pending.Version != "v0.2.0" || st.Pending.File != want {
				t.Fatalf("pending = %+v", st.Pending)
			}
			if exists(oldPending) {
				t.Fatal("the superseded pending file was not deleted")
			}
			if parts, _ := filepath.Glob(filepath.Join(updDir, "*.part")); len(parts) != 0 {
				t.Fatalf(".part left behind: %v", parts)
			}
		})
	}
}

// roundTripFunc adapts a function to an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchUnsafeVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request expected for an unsafe version")
	}))
	defer srv.Close()
	body := realBinary(t)
	m := manifestFor(t, "../evil", ownKey(t), body)
	inst := &Installer{Dir: t.TempDir()}
	if _, err := inst.Fetch(context.Background(), srv.URL, m, ownKey(t)); err == nil {
		t.Fatal("Fetch accepted an unsafe version")
	}
}

func TestFetchConcurrent(t *testing.T) {
	body := realBinary(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	_, binDir, _ := binDirs(t)
	m := manifestFor(t, "v0.2.0", ownKey(t), body)
	inst := &Installer{Dir: binDir}
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := inst.Fetch(context.Background(), srv.URL, m, ownKey(t))
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Fetch: %v", err)
		}
	}
	st := mustLoad(t, binDir)
	if st.Pending == nil || st.Pending.Version != "v0.2.0" {
		t.Fatalf("pending = %+v", st.Pending)
	}
	if _, err := os.Stat(BinaryName(binDir, "v0.2.0")); err != nil {
		t.Fatal(err)
	}
}

// --- Checker ---------------------------------------------------------------

// release serves a manifest and its signature over httptest.
type release struct {
	manifest []byte
	sig      []byte
	// manifestStatus/sigStatus force an HTTP error when non-zero.
	manifestStatus, sigStatus int
	manifestHits, sigHits     atomic.Int64
}

func (r *release) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, ".ed25519"):
			r.sigHits.Add(1)
			if r.sigStatus != 0 {
				http.Error(w, "nope", r.sigStatus)
				return
			}
			_, _ = w.Write(r.sig)
		case strings.HasSuffix(req.URL.Path, "/manifest.json"):
			r.manifestHits.Add(1)
			if r.manifestStatus != 0 {
				http.Error(w, "nope", r.manifestStatus)
				return
			}
			_, _ = w.Write(r.manifest)
		default:
			http.NotFound(w, req)
		}
	})
}

func signedRelease(t *testing.T, version string) (*release, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(strings.Replace(goodManifestJSON(""), "v0.2.0", version, 1))
	return &release{manifest: body, sig: []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, body)) + "\n")}, pub
}

func newChecker(t *testing.T, url string, pub ed25519.PublicKey, clock *time.Time) (*Checker, string) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "update-cache.json")
	c := &Checker{
		ManifestURL: url + "/manifest.json",
		Pub:         pub,
		CachePath:   cache,
		TTL:         time.Hour,
		Now:         func() time.Time { return *clock },
	}
	return c, cache
}

func TestCheckerLatest(t *testing.T) {
	t.Run("signed", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, cache := newChecker(t, srv.URL, pub, &clock)
		m, err := c.Latest(context.Background())
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if m.Version != "v0.2.0" {
			t.Fatalf("version = %q", m.Version)
		}
		if !exists(cache) {
			t.Fatal("cache file was not written")
		}
		fi, err := os.Stat(cache)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("cache mode = %o, want 600", perm)
		}
		if got := c.Cached(); got == nil || got.Version != "v0.2.0" {
			t.Fatalf("Cached() = %+v", got)
		}
	})
	t.Run("unsigned", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.3.0")
		rel.sigStatus = http.StatusNotFound
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest accepted an unsigned manifest")
		}
		if c.Cached() != nil {
			t.Fatal("Cached() returned a manifest after a failure")
		}
	})
	t.Run("bad signature", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.3.0")
		rel.sig = []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest accepted a bad signature")
		}
	})
	t.Run("tampered manifest", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.3.0")
		rel.manifest = []byte(strings.Replace(string(rel.manifest), `"size":12345`, `"size":1`, 1))
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest accepted a tampered manifest")
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		rel, _ := signedRelease(t, "v0.3.0")
		otherPub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, otherPub, &clock)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest accepted a manifest signed by another key")
		}
	})
	t.Run("no public key", func(t *testing.T) {
		rel, _ := signedRelease(t, "v0.3.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, nil, &clock)
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest ran without a public key")
		}
		if rel.manifestHits.Load() != 0 {
			t.Fatal("Latest hit the network without a public key")
		}
	})
	t.Run("ttl cache hit makes no second request", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		m, err := c.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != "v0.2.0" {
			t.Fatalf("version = %q", m.Version)
		}
		if got := rel.manifestHits.Load(); got != 1 {
			t.Fatalf("manifest requests = %d, want 1 within TTL", got)
		}
		if got := rel.sigHits.Load(); got != 1 {
			t.Fatalf("signature requests = %d, want 1 within TTL", got)
		}
		// Past the TTL the check runs again.
		clock = clock.Add(61 * time.Minute)
		if _, err := c.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := rel.manifestHits.Load(); got != 2 {
			t.Fatalf("manifest requests = %d, want 2 after the TTL", got)
		}
		if c.Cached() == nil {
			t.Fatal("Cached() empty after a fresh check")
		}
	})
	t.Run("refresh ignores the cache", func(t *testing.T) {
		// "Check now" after a release landed inside the TTL must see it: the
		// cache is for the background path, not for the button.
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, _ := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := rel.manifestHits.Load(); got != 1 {
			t.Fatalf("manifest requests = %d, want 1", got)
		}
		m, err := c.Refresh(context.Background())
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if m.Version != "v0.2.0" {
			t.Fatalf("version = %q", m.Version)
		}
		if got := rel.manifestHits.Load(); got != 2 {
			t.Fatalf("manifest requests = %d, want 2 within the TTL after Refresh", got)
		}
	})
	t.Run("error leaves the cache", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c, cache := newChecker(t, srv.URL, pub, &clock)
		if _, err := c.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(cache)
		if err != nil {
			t.Fatal(err)
		}
		// Expire the cache and break the server.
		clock = clock.Add(2 * time.Hour)
		rel.manifestStatus = http.StatusInternalServerError
		if _, err := c.Latest(context.Background()); err == nil {
			t.Fatal("Latest succeeded against a failing server")
		}
		after, err := os.ReadFile(cache)
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("a failed check overwrote the cache file")
		}
		// The last known manifest stays available offline (the UI shows it
		// with its age); only Latest is expired.
		if got := c.Cached(); got == nil || got.Version != "v0.2.0" {
			t.Fatalf("Cached() after an expiry + failure = %+v", got)
		}
	})
	t.Run("disk cache is used when fresh", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		c1, cache := newChecker(t, srv.URL, pub, &clock)
		if _, err := c1.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		// A new process (new Checker, same cache path) within TTL.
		c2 := &Checker{ManifestURL: srv.URL + "/manifest.json", Pub: pub, CachePath: cache,
			TTL: time.Hour, Now: func() time.Time { return clock }}
		m, err := c2.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != "v0.2.0" {
			t.Fatalf("version = %q", m.Version)
		}
		if got := rel.manifestHits.Load(); got != 1 {
			t.Fatalf("manifest requests = %d, want 1 (disk cache)", got)
		}
		if c2.Cached() == nil {
			t.Fatal("Cached() empty from the disk cache")
		}
	})
	t.Run("tampered disk cache is ignored", func(t *testing.T) {
		rel, pub := signedRelease(t, "v0.2.0")
		srv := httptest.NewServer(rel.handler())
		defer srv.Close()
		clock := testNow
		cache := filepath.Join(t.TempDir(), "update-cache.json")
		// A cache claiming a newer version, signed with nothing.
		entry := cachedManifest{
			FetchedAt: testNow,
			Manifest:  base64.StdEncoding.EncodeToString([]byte(strings.Replace(goodManifestJSON(""), "v0.2.0", "v9.9.9", 1))),
			Signature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
		}
		b, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cache, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Checker{ManifestURL: srv.URL + "/manifest.json", Pub: pub, CachePath: cache,
			TTL: time.Hour, Now: func() time.Time { return clock }}
		if got := c.Cached(); got != nil {
			t.Fatalf("Cached() trusted a forged cache: %+v", got)
		}
		m, err := c.Latest(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != "v0.2.0" {
			t.Fatalf("version = %q, want the real release", m.Version)
		}
	})
}

// TestCheckerCacheKeepsServedBytes pins the cache format: the manifest has to
// be stored byte for byte, because the signature covers the bytes (whitespace
// included) and a JSON re-encoding would re-indent them and break the next
// start's verification.
func TestCheckerCacheKeepsServedBytes(t *testing.T) {
	rel, pub := signedRelease(t, "v0.2.0")
	// A manifest with unusual but legal whitespace.
	rel.manifest = []byte(strings.Replace(string(rel.manifest), `"schema":1`, "\n\t\"schema\" :  1", 1))
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()
	clock := testNow
	c, cache := newChecker(t, srv.URL, pub, &clock)
	// The whitespace change breaks the signature: sign again with the key of
	// the same release by starting from a fresh pair.
	pub2, priv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rel.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv2, rel.manifest)))
	c.Pub = pub2
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatalf("Latest: %v", err)
	}
	b, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var e cachedManifest
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	if got := string(e.manifestBytes()); got != string(rel.manifest) {
		t.Fatalf("cache manifest differs from the served bytes:\n got %q\nwant %q", got, rel.manifest)
	}
	// A second process reading only the cache verifies it.
	c2 := &Checker{ManifestURL: srv.URL + "/manifest.json", Pub: pub2, CachePath: cache,
		TTL: time.Hour, Now: func() time.Time { return clock }}
	if got := c2.Cached(); got == nil || got.Version != "v0.2.0" {
		t.Fatalf("Cached() from disk = %+v", got)
	}
	if hits := rel.manifestHits.Load(); hits != 1 {
		t.Fatalf("manifest requests = %d, want 1", hits)
	}
}

func TestCheckerBadSignatureEncoding(t *testing.T) {
	rel, pub := signedRelease(t, "v0.2.0")
	rel.sig = []byte("!!! not base64 !!!")
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()
	clock := testNow
	c, _ := newChecker(t, srv.URL, pub, &clock)
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest accepted an undecodable signature")
	}
}

// TestFetchRefusesWhenTheDiskIsFull pins the check that turns a half-written
// download into one sentence: a nearly full /data used to fail in the middle,
// looking like a network problem.
func TestFetchRefusesWhenTheDiskIsFull(t *testing.T) {
	rel, pub := signedRelease(t, "v0.2.0")
	_ = pub
	srv := httptest.NewServer(rel.handler())
	defer srv.Close()
	dir := t.TempDir()
	inst := &Installer{
		Dir:       filepath.Join(dir, "bin"),
		Client:    srv.Client(),
		FreeSpace: func(string) (int64, error) { return 1024, nil },
	}
	m := &Manifest{
		Schema:  1,
		Version: "v0.2.0",
		Assets:  map[string]Asset{"linux-amd64": {File: "wa-transcriber-linux-amd64", SHA256: strings.Repeat("a", 64), Size: 8 << 20}},
	}
	_, err := inst.Fetch(context.Background(), srv.URL+"/asset", m, "linux-amd64")
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("Fetch = %v, want a free-space refusal", err)
	}
	if rel.manifestHits.Load() != 0 {
		t.Fatal("Fetch downloaded something before checking the space")
	}
	if got := dirEntries(t, filepath.Join(dir, "bin")); len(got) != 0 {
		t.Fatalf("something was written: %v", got)
	}
}

// dirEntries lists a directory, or nothing when it does not exist.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
