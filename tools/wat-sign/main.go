// Command wat-sign signs a release manifest with the owner's ed25519 key
// (docs/ARCHITECTURE.md section 11, decision D17).
//
// It reads the manifest bytes and a private key file written by
// tools/wat-keygen, and writes the detached signature as base64 of the
// 64-byte ed25519 signature on a single line plus a trailing newline — the
// exact format internal/update expects for manifest.json.ed25519.
//
// The signing is offline on purpose: CI never sees the private key.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	key := flag.String("key", "", "path of the private key file from wat-keygen (required)")
	in := flag.String("in", "manifest.json", "manifest file to sign")
	out := flag.String("out", "", "signature file to write (default: <in>.ed25519)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: wat-sign -key <private-key> -in manifest.json -out manifest.json.ed25519\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	outPath := *out
	if outPath == "" {
		outPath = *in + ".ed25519"
	}

	if err := run(*key, *in, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "wat-sign: %v\n", err)
		os.Exit(1)
	}
}

func run(keyPath, inPath, outPath string) error {
	if keyPath == "" {
		return fmt.Errorf("-key is required")
	}

	priv, err := loadKey(keyPath)
	if err != nil {
		return err
	}

	msg, err := os.ReadFile(inPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}

	sig := ed25519.Sign(priv, msg)
	line := base64.StdEncoding.EncodeToString(sig) + "\n"
	if err := os.WriteFile(outPath, []byte(line), 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	return nil
}

// loadKey reads a wat-keygen private key file: comment lines starting with
// '#' are skipped, the first remaining non-blank line is the base64 of the
// 32-byte ed25519 seed.
func loadKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}

	var encoded string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		encoded = line
		break
	}
	if encoded == "" {
		return nil, fmt.Errorf("%s: no base64 seed line found", path)
	}

	seed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s: decode seed: %w", path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: seed is %d bytes, want %d", path, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
