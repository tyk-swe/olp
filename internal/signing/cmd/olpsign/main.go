// Command olpsign signs and verifies the documents a release ships: the
// reference catalog and the plugin index. It is a maintainer and CI tool and is
// not part of the olp binary.
//
//	olpsign keygen
//	olpsign fmt [-check] DOCUMENT...
//	olpsign sign -key-id ID [-seed-file FILE] DOCUMENT...
//	olpsign verify DOCUMENT...
//
// fmt rewrites each document in its canonical form, the exact bytes a
// signature covers; with -check it only reports a document that is not.
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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tyk-swe/olp/internal/catalog"
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
		return errors.New("usage: olpsign keygen | fmt [-check] DOCUMENT... | sign -key-id ID [-seed-file FILE] DOCUMENT... | verify DOCUMENT...")
	}
	switch args[0] {
	case "keygen":
		return keygen(stdout)
	case "fmt":
		return format(args[1:], stdout)
	case "sign":
		return sign(args[1:], stdout)
	case "verify":
		return verify(args[1:], stdout)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

// canonicalizers render each signed document format canonically, by its
// api_version.
var canonicalizers = map[string]func([]byte) ([]byte, error){
	catalog.APIVersion: catalog.Canonical,
}

func format(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fmt", flag.ContinueOnError)
	check := flags.Bool("check", false, "report documents that are not canonical instead of rewriting them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for _, path := range flags.Args() {
		document, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var version struct {
			APIVersion string `json:"api_version"`
		}
		if err := json.Unmarshal(document, &version); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		canonicalize, ok := canonicalizers[version.APIVersion]
		if !ok {
			return fmt.Errorf("%s: no signed document format %q", path, version.APIVersion)
		}
		canonical, err := canonicalize(document)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		switch {
		case bytes.Equal(canonical, document):
		case *check:
			return fmt.Errorf("%s is not canonical; run make catalog", path)
		default:
			if err := os.WriteFile(path, canonical, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "formatted %s\n", path)
		}
	}
	return nil
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
