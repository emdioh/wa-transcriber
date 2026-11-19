package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func captiveHandler(t *testing.T) (*Server, func(bool)) {
	t.Helper()
	h := newHarness(t)
	ap := false
	h.srv.Close() // replace the harness server with one that has InAPMode
	s := New(Options{
		Config:   h.cfg,
		Status:   h.st,
		Logs:     h.logs,
		WA:       h.wa,
		Getenv:   func(k string) string { return h.env[k] },
		Version:  "test",
		InAPMode: func() bool { return ap },
	})
	return s, func(v bool) { ap = v }
}

func TestCaptivePortalRedirectsOnlyInAPMode(t *testing.T) {
	s, setAP := captiveHandler(t)
	h := s.Handler()

	do := func(host, path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Not in AP mode: no redirect, the connectivity path is just a 404.
	if code := do("192.168.4.1", "/generate_204"); code != http.StatusNotFound {
		t.Fatalf("without AP: %d", code)
	}
	setAP(true)

	for _, path := range []string{"/generate_204", "/gen_204", "/hotspot-detect.html",
		"/library/test/success.html", "/connecttest.txt", "/ncsi.txt"} {
		if code := do("192.168.4.1", path); code != http.StatusFound {
			t.Errorf("%s in AP mode: %d, want 302", path, code)
		}
	}
	// A request for a foreign host is redirected to the portal.
	if code := do("connectivitycheck.gstatic.com", "/"); code != http.StatusFound {
		t.Errorf("foreign host: %d, want 302", code)
	}
	// The right host on a normal path is served.
	if code := do("192.168.4.1", "/setup"); code != http.StatusOK {
		t.Errorf("setup page on AP host: %d, want 200", code)
	}
	if code := do("wa.local:80", "/setup"); code != http.StatusOK {
		t.Errorf("setup page on wa.local: %d, want 200", code)
	}
}

func TestCaptiveRedirectTarget(t *testing.T) {
	s, setAP := captiveHandler(t)
	setAP(true)
	req := httptest.NewRequest(http.MethodGet, "/generate_204", nil)
	req.Host = "192.168.4.1"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if loc := rec.Header().Get("Location"); loc != "http://192.168.4.1/" {
		t.Fatalf("Location = %q", loc)
	}
}
