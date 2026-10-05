// Command olpsign signs and verifies the documents a release ships: the
// reference catalog and the plugin index. It is a maintainer and CI tool and is
// not part of the olp binary.
//
//	olpsign keygen
//	olpsign sign -key-id ID [-seed-file FILE] DOCUMENT...
//	olpsign verify DOCUMENT...
//
// sign reads the base64 Ed25519 seed from OLP_SIGNING_KEY, or from -seed-file,
// and writes DOCUMENT.sig beside each document, keeping other keys'
// signatures. verify checks each DOCUMENT.sig against the keys this build
// trusts: build with -tags release to verify as a release binary does.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tyk-swe/olp/internal/signing"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "olpsign:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: olpsign keygen | sign -key-id ID [-seed-file FILE] DOCUMENT... | verify DOCUMENT...")
	}
	switch args[0] {
	case "keygen":
		return keygen(stdout)
	case "sign":
		return sign(args[1:], stdout)
	case "verify":
		return verify(args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func keygen(stdout io.Writer) error {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "public key (commit to internal/signing/keys.go): %s\n", base64.StdEncoding.EncodeToString(public))
	fmt.Fprintf(stdout, "seed (store as the OLP_SIGNING_KEY secret; never commit): %s\n", base64.StdEncoding.EncodeToString(private.Seed()))
	return nil
}

func sign(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	id := flags.String("key-id", "", "identifier of the signing key")
	seedFile := flags.String("seed-file", "", "file holding the base64 seed, instead of OLP_SIGNING_KEY")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("name at least one document to sign")
	}
	encoded := os.Getenv("OLP_SIGNING_KEY")
	if *seedFile != "" {
		raw, err := os.ReadFile(*seedFile)
		if err != nil {
			return err
		}
		encoded = string(raw)
	}
	if strings.TrimSpace(encoded) == "" {
		return errors.New("set OLP_SIGNING_KEY or -seed-file to the signing seed")
	}
	private, err := signing.ParseSeed(strings.TrimSpace(encoded))
	if err != nil {
		return err
	}
	known, ok := signing.Known(*id)
	if !ok {
		return fmt.Errorf("key %q is not compiled into this build; add its public key to internal/signing/keys.go first", *id)
	}
	if !bytes.Equal(private.Public().(ed25519.PublicKey), known.Public) {
		return fmt.Errorf("the seed is not the private half of key %q", *id)
	}
	for _, path := range flags.Args() {
		document, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		existing, err := os.ReadFile(path + ".sig")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		signatures, err := signing.Sign(existing, *id, private, document)
		if errors.Is(err, signing.ErrMalformedSignature) {
			// A stale or damaged file is replaced rather than merged.
			signatures, err = signing.Sign(nil, *id, private, document)
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".sig", signatures, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed %s with %s\n", path, *id)
	}
	return nil
}

func verify(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("name at least one document to verify")
	}
	for _, path := range args {
		document, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		signatures, err := os.ReadFile(path + ".sig")
		if err != nil {
			return err
		}
		id, err := signing.Trusted().Verify(document, signatures)
		if err != nil {
			return fmt.Errorf("%s: %w (trusted keys in this %s build: %v)", path, err, signing.Channel, signing.Trusted().IDs())
		}
		fmt.Fprintf(stdout, "%s verified by %s\n", path, id)
	}
	return nil
}
