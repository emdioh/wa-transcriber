package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// healthcheckTimeout bounds one probe. Docker's HEALTHCHECK has its own
// timeout and retries, so a quick failure is better than a stuck probe.
const healthcheckTimeout = 5 * time.Second

// healthcheckURL turns the -listen address into the local health URL. The
// probe always talks to 127.0.0.1: the UI may listen on :80, :8080 or a
// specific interface, but Docker runs the probe inside the same container.
func healthcheckURL(listen string) (string, error) {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		return "", errors.New("-healthcheck: the web UI is disabled (-listen is empty)")
	}
	port := addr
	if _, p, err := net.SplitHostPort(addr); err == nil {
		port = p
	} else if strings.Contains(addr, ":") {
		return "", fmt.Errorf("-healthcheck: bad -listen address %q", listen)
	}
	if port == "" {
		return "", fmt.Errorf("-healthcheck: no port in -listen address %q", listen)
	}
	return "http://127.0.0.1:" + port + "/healthz", nil
}

// healthcheck reports whether the local web UI answers GET /healthz with 200.
// A nil client gets a client with healthcheckTimeout.
func healthcheck(listen string, client *http.Client) error {
	url, err := healthcheckURL(listen)
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{Timeout: healthcheckTimeout}
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
