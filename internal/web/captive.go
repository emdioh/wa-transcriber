package web

import (
	"net"
	"net/http"
)

// apIP is the setup AP's own address; a captive portal redirects here.
const apIP = "192.168.4.1"

// captivePaths are the connectivity-check endpoints of Android, Apple and
// Windows. While the setup AP runs, all of them redirect to the UI.
var captivePaths = map[string]bool{
	"/generate_204":              true,
	"/gen_204":                   true,
	"/hotspot-detect.html":       true,
	"/library/test/success.html": true,
	"/connecttest.txt":           true,
	"/ncsi.txt":                  true,
	"/redirect":                  true,
	"/canonical.html":            true,
	"/success.txt":               true,
	"/captiveportal":             true,
}

// captivePortal answers the OS connectivity checks and any request whose Host
// is not the device, so a phone that joins the setup AP shows the UI by
// itself. It is a no-op unless the AP is running.
func (s *Server) captivePortal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.InAPMode == nil || !s.opts.InAPMode() {
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		known := host == apIP || host == "wa.local" || host == "" || host == "localhost"
		if !known || captivePaths[r.URL.Path] {
			http.Redirect(w, r, "http://"+apIP+"/", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}
