package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/backup"
	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/status"
)

// backupHarness is the real server over a real data directory: the backup
// code writes and reads files, so a fake would test nothing.
type backupHarness struct {
	*harness
	data    string
	rMu     sync.Mutex
	restart int
}

func newBackupHarness(t *testing.T) *backupHarness {
	t.Helper()
	data := t.TempDir()
	cfg, err := config.Open(filepath.Join(data, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, st: status.New(time.Now()), wa: &fakeWA{},
		logs: logbuf.New(1<<16, nil), env: map[string]string{}}
	b := &backupHarness{harness: h, data: data}
	s := New(Options{
		Config: cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:  func(k string) string { return h.env[k] },
		Addrs:   func() []status.Addr { return nil },
		Version: "v9.9.9",
		DataDir: data,
		Restart: func() { b.rMu.Lock(); b.restart++; b.rMu.Unlock() },
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	return b
}

func (b *backupHarness) restarts() int {
	b.rMu.Lock()
	defer b.rMu.Unlock()
	return b.restart
}

// waitRestarts polls the counter under the mutex: the restart runs in a
// goroutine after the response, so reading the field directly would race.
func (b *backupHarness) waitRestarts(want int) int {
	b.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.restarts() >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return b.restarts()
}

// seed gives the device a state worth backing up.
func (b *backupHarness) seed(t *testing.T) {
	t.Helper()
	b.setup()
	for name, body := range map[string]string{
		"session.db":     "session bytes",
		"session.db-wal": "wal",
		"wifi.json":      `{"networks":[{"ssid":"home","psk":"secret"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(b.data, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// download fetches the archive and returns the response and its body.
func (b *backupHarness) download(c *http.Client) (*http.Response, []byte) {
	b.t.Helper()
	resp, err := c.Get(b.srv.URL + "/system/backup")
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.t.Fatal(err)
	}
	return resp, body
}

// upload posts an archive as the multipart field "file" with a same-site
// Origin.
func (b *backupHarness) upload(c *http.Client, path string, archive []byte) (int, string, string) {
	b.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "backup.tar.gz")
	if err != nil {
		b.t.Fatal(err)
	}
	fw.Write(archive)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, b.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", b.srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL.Path, string(body)
}

// archiveOf builds a backup of the current data directory.
func (b *backupHarness) archiveOf(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := backup.Write(&buf, b.data, backup.Meta{Version: "v9.9.9", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBackupDownloadNeedsASession(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	anon := b.newClient()
	resp, err := anon.Get(b.srv.URL + "/system/backup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("anonymous download reached %s, want /login", resp.Request.URL.Path)
	}
	if resp.Request.URL.Query().Get("next") != "/system/backup" {
		t.Fatalf("next = %q", resp.Request.URL.Query().Get("next"))
	}
}

func TestBackupDownloadIsAnArchiveWithItsManifestFirst(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	resp, body := b.download(b.c)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", resp.StatusCode, body)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="wa-transcriber-v9.9.9-`) || !strings.HasSuffix(cd, `.tar.gz"`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		t.Fatalf("first entry %v %v, want manifest.json", hdr, err)
	}
	meta, err := backup.Verify(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"config.json", "session.db", "session.db-wal", "wifi.json"} {
		if _, ok := meta.Files[want]; !ok {
			t.Fatalf("the download does not hold %s: %v", want, meta.Files)
		}
	}
	if meta.Version != "v9.9.9" {
		t.Fatalf("version = %q", meta.Version)
	}
	// The card and its warning.
	_, _, page := b.get(b.c, "/system")
	for _, want := range []string{"Backup", "Download the backup", "transcription API key",
		"WhatsApp session", "WiFi passwords", `/system/restore`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the card does not say %q", want)
		}
	}
}

func TestRestoreWithASessionRestartsOnce(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	_, archive := b.download(b.c)
	// Something that must be undone by the restore.
	os.WriteFile(filepath.Join(b.data, "session.db"), []byte("clobbered"), 0o644)
	os.Remove(filepath.Join(b.data, "wifi.json"))

	code, path, page := b.upload(b.c, "/system/restore", archive)
	// restartPage answers in place, so the path is still the POST's.
	if code != http.StatusOK || path != "/system/restore" {
		t.Fatalf("restore: %d %s (%s)", code, path, page)
	}
	if !strings.Contains(page, "Restarting") {
		t.Fatalf("not the restart page:\n%s", page)
	}
	if got := b.waitRestarts(1); got != 1 {
		t.Fatalf("restarts = %d, want 1", got)
	}
	if got, err := os.ReadFile(filepath.Join(b.data, "session.db")); err != nil || string(got) != "session bytes" {
		t.Fatalf("session.db = %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(b.data, "wifi.json")); err != nil {
		t.Fatalf("wifi.json was not restored: %v", err)
	}
	// Nothing from the staging directories is left behind.
	entries, _ := os.ReadDir(b.data)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("%q was left in the data directory", e.Name())
		}
	}
}

func TestRestoreRefusesABadArchiveAndDoesNotRestart(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	_, archive := b.download(b.c)
	// Corrupt the compressed stream in the middle: a truncated upload.
	bad := archive[:len(archive)/2]

	before := dirContents(t, b.data)
	code, _, page := b.upload(b.c, "/system/restore", bad)
	if code != http.StatusBadRequest {
		t.Fatalf("bad archive: %d, want 400", code)
	}
	if !strings.Contains(page, "cannot be restored") {
		t.Fatalf("no reason given:\n%s", page)
	}
	time.Sleep(2 * updateRestartDelay)
	if got := b.restarts(); got != 0 {
		t.Fatalf("a refused restore restarted the device %d times", got)
	}
	if after := dirContents(t, b.data); !sameContents(before, after) {
		t.Fatalf("the data directory changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSetupPageRestoresWithoutAPassword(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	_, archive := b.download(b.c)

	// Forget the password, as a reflash does: a fresh data directory.
	fresh := t.TempDir()
	b.data = fresh
	cfg, err := config.Open(filepath.Join(fresh, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.cfg = cfg
	s := New(Options{
		Config: cfg, Status: b.st, Logs: b.logs, WA: b.wa,
		Getenv:  func(k string) string { return b.env[k] },
		Version: "v9.9.9",
		DataDir: fresh,
		Restart: func() { b.rMu.Lock(); b.restart++; b.rMu.Unlock() },
	})
	b.srv.Close()
	b.srv = httptest.NewServer(s.Handler())
	t.Cleanup(b.srv.Close)
	b.c = b.newClient()

	// The setup page offers it.
	if _, _, page := b.get(b.c, "/setup"); !strings.Contains(page, "Restore a backup") ||
		!strings.Contains(page, `action="/restore"`) {
		t.Fatalf("the setup page has no restore section:\n%s", page)
	}

	if code, _, page := b.upload(b.c, "/restore", archive); code != http.StatusOK ||
		!strings.Contains(page, "Restarting") {
		t.Fatalf("setup restore: %d\n%s", code, page)
	}
	if got := b.waitRestarts(1); got != 1 {
		t.Fatalf("restarts = %d", got)
	}
	if _, err := os.Stat(filepath.Join(fresh, "session.db")); err != nil {
		t.Fatalf("the session was not restored: %v", err)
	}
	// The archived config.json is the device's again. The Store object in
	// this test is not the running one (the process restarts), so read it.
	stored, err := config.Open(filepath.Join(fresh, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Get().Web.PasswordHash == "" {
		t.Fatal("the restored config.json has no password")
	}
}

func TestSetupRestoreIsRefusedOnceAPasswordExists(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	_, archive := b.download(b.c)
	// A device with a password: the setup route is not a second way in.
	anon := b.newClient()
	before := dirContents(t, b.data)
	code, path, page := b.upload(anon, "/restore", archive)
	if code != http.StatusOK || path != "/login" {
		t.Fatalf("restore on a claimed device: %d %s", code, path)
	}
	if !strings.Contains(page, "System page") {
		t.Fatalf("the refusal does not point at the System page:\n%s", page)
	}
	if after := dirContents(t, b.data); !sameContents(before, after) {
		t.Fatal("a refused restore changed the data directory")
	}
	time.Sleep(2 * updateRestartDelay)
	if got := b.restarts(); got != 0 {
		t.Fatalf("restarts = %d", got)
	}
	// The section is gone from the page too.
	if _, path, page := b.get(anon, "/setup"); path != "/login" || strings.Contains(page, "Restore a backup") {
		t.Fatalf("the setup page still offers a restore: %s", path)
	}
}

func TestSetupRestoreIsRefusedWhilePaired(t *testing.T) {
	// No password but a linked device: never touched without the password,
	// the same rule as the login page's reset.
	b := newBackupHarness(t)
	_, archive := b.download(b.newClient())
	b.wa.paired = true
	before := dirContents(t, b.data)
	if code, _, _ := b.upload(b.newClient(), "/restore", archive); code != http.StatusOK {
		t.Fatalf("restore on a paired, unclaimed device: %d", code)
	}
	if after := dirContents(t, b.data); !sameContents(before, after) {
		t.Fatal("a refused restore changed the data directory")
	}
	if got := b.restarts(); got != 0 {
		t.Fatalf("restarts = %d", got)
	}
}

func TestRestoreRefusesAForeignOrigin(t *testing.T) {
	b := newBackupHarness(t)
	b.seed(t)
	_, archive := b.download(b.c)
	for _, path := range []string{"/system/restore", "/restore"} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "backup.tar.gz")
		fw.Write(archive)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, b.srv.URL+path, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", "http://evil.example")
		resp, err := b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s from a foreign origin: %d, want 403", path, resp.StatusCode)
		}
	}
	time.Sleep(2 * updateRestartDelay)
	if got := b.restarts(); got != 0 {
		t.Fatalf("restarts = %d", got)
	}
}

func TestBackupRoutesNeedADataDir(t *testing.T) {
	h := newHarness(t) // no DataDir
	h.setup()
	if code, _, _ := h.get(h.c, "/system/backup"); code != http.StatusNotFound {
		t.Fatalf("download without a data directory: %d", code)
	}
	_, _, page := h.get(h.c, "/system")
	if strings.Contains(page, "Download the backup") {
		t.Fatal("the card is rendered without a data directory")
	}
}

// dirContents snapshots a directory: names, sizes and contents.
func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			out[e.Name()] = "<unreadable>"
			continue
		}
		out[e.Name()] = string(b)
	}
	return out
}

func sameContents(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
