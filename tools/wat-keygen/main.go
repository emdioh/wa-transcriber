// Command wat-keygen generates the ed25519 keypair used to sign release
// manifests (docs/ARCHITECTURE.md section 11, decision D17).
//
// The private key file holds a comment line plus the base64 of the 32-byte
// seed; tools/wat-sign reads the same format. The public key is printed on
// stdout in base64, ready to be pasted into release/update-pubkey.txt.
//
// The private key is generated here and must stay on the owner's machine:
// it is never committed, it is not backed up anywhere, and a lost key means
// every device must be reflashed with a binary that embeds the new one.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// keyHeader opens every private key file. wat-sign skips comment lines.
const keyHeader = "# wa-transcriber update signing key (ed25519 seed, base64) - KEEP PRIVATE, NEVER COMMIT"

func main() {
	out := flag.String("out", "", "path of the private key file to create (required, must not exist)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: wat-keygen -out <path>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "wat-keygen: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}

	// Refuse to overwrite: regenerating a key silently would leave existing
	// releases signed with a key whose public half is no longer embedded.
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", out)
	} else if !os.IsNotExist(err) {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	seed := priv.Seed()

	content := keyHeader + "\n" + base64.StdEncoding.EncodeToString(seed) + "\n"

	// O_CREATE|O_EXCL so a file that appears between the stat above and the
	// write still triggers a refusal instead of a silent overwrite.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", out, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", out, err)
	}

	abs, err := filepath.Abs(out)
	if err != nil {
		abs = out
	}

	fmt.Println(base64.StdEncoding.EncodeToString(pub))
	fmt.Fprintf(os.Stderr, "wat-keygen: private key written to %s (mode 0600)\n", abs)
	fmt.Fprintf(os.Stderr, "wat-keygen: the private key must never enter the repository, cannot be recovered,\n")
	fmt.Fprintf(os.Stderr, "wat-keygen: and any device that does not embed its public half will refuse the update.\n")
	fmt.Fprintf(os.Stderr, "wat-keygen: keep it offline; paste the line above into release/update-pubkey.txt.\n")
	return nil
}
