package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHealthcheckURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
		err    bool
	}{
		{":8080", "http://127.0.0.1:8080/healthz", false},
		{"8080", "http://127.0.0.1:8080/healthz", false},
		{"0.0.0.0:80", "http://127.0.0.1:80/healthz", false},
		{"192.168.1.2:9000", "http://127.0.0.1:9000/healthz", false},
		{"[::]:8080", "http://127.0.0.1:8080/healthz", false},
		{"", "", true},
		{":", "", true},
		{"::1:8080", "", true},
	}
	for _, tt := range tests {
		got, err := healthcheckURL(tt.listen)
		if tt.err {
			if err == nil {
				t.Errorf("healthcheckURL(%q) = %q, want error", tt.listen, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("healthcheckURL(%q): %v", tt.listen, err)
			continue
		}
		if got != tt.want {
			t.Errorf("healthcheckURL(%q) = %q, want %q", tt.listen, got, tt.want)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer healthy.Close()
	if err := healthcheck(healthy.Listener.Addr().String(), nil); err != nil {
		t.Fatalf("healthy server: %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	err := healthcheck(unhealthy.Listener.Addr().String(), nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("unhealthy server: %v", err)
	}

	// Nothing listening: a listener closed right away keeps the port free.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	if err := healthcheck(addr, &http.Client{}); err == nil {
		t.Fatal("closed port reported healthy")
	}

	if err := healthcheck("", nil); err == nil {
		t.Fatal("empty -listen reported healthy")
	}
}

func TestHealthcheckURLAcceptsServerURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := healthcheck(u.Host, nil); err != nil {
		t.Fatal(err)
	}
}
