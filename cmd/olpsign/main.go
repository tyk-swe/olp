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
	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/signing"
)

const usage = `usage: olpsign keygen | fmt [-check] DOCUMENT... | sign -key-id ID [-seed-file FILE] DOCUMENT... | verify DOCUMENT...

fmt rewrites each document in its canonical form, the exact bytes a signature covers. With -check it only reports a document that is not.
sign reads the base64 Ed25519 seed from OLP_SIGNING_KEY or -seed-file and writes DOCUMENT.sig beside each document, keeping other keys' signatures.
verify checks each DOCUMENT.sig against the keys this build trusts. Build with -tags release to verify as a release binary does.`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "olpsign:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
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

func canonicalize(apiVersion string, document []byte) ([]byte, error) {
	switch apiVersion {
	case catalog.APIVersion:
		return catalog.Canonical(document)
	case pluginindex.APIVersion:
		return pluginindex.Canonical(document)
	}
	return nil, fmt.Errorf("no signed document format %q", apiVersion)
}

func format(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("fmt", flag.ContinueOnError)
	check := flags.Bool("check", false, "report documents that are not canonical instead of rewriting them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for _, path := range flags.Args() {
		if err := formatDocument(path, *check, stdout); err != nil {
			return err
		}
	}
	return nil
}

func formatDocument(path string, check bool, stdout io.Writer) error {
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
	canonical, err := canonicalize(version.APIVersion, document)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(canonical, document) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is not canonical; run make catalog", path)
	}
	if err := os.WriteFile(path, canonical, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "formatted %s\n", path)
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
	seed, err := readSeed(*seedFile)
	if err != nil {
		return err
	}
	private, err := signing.ParseSeed(seed)
	if err != nil {
		return err
	}
	known, err := signing.Known(*id)
	if err != nil {
		return err
	}
	if !bytes.Equal(private.Public().(ed25519.PublicKey), known.Public) {
		return fmt.Errorf("the seed is not the private half of key %q", *id)
	}
	for _, path := range flags.Args() {
		if err := signDocument(path, *id, private, stdout); err != nil {
			return err
		}
	}
	return nil
}

func readSeed(seedFile string) (string, error) {
	if seedFile == "" {
		return nonEmptySeed(os.Getenv("OLP_SIGNING_KEY"))
	}
	raw, err := os.ReadFile(seedFile)
	if err != nil {
		return "", err
	}
	return nonEmptySeed(string(raw))
}

func nonEmptySeed(encoded string) (string, error) {
	seed := strings.TrimSpace(encoded)
	if seed == "" {
		return "", errors.New("set OLP_SIGNING_KEY or -seed-file to the signing seed")
	}
	return seed, nil
}

func signDocument(path, id string, private ed25519.PrivateKey, stdout io.Writer) error {
	document, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(path + ".sig")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	signatures, err := signatureFile(existing, id, private, document)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".sig", signatures, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "signed %s with %s\n", path, id)
	return nil
}

func signatureFile(existing []byte, id string, private ed25519.PrivateKey, document []byte) ([]byte, error) {
	merged, err := signing.Sign(existing, id, private, document)
	if !errors.Is(err, signing.ErrMalformedSignature) {
		return merged, err
	}
	return signing.Sign(nil, id, private, document)
}

func verify(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("name at least one document to verify")
	}
	keys, err := signing.Trusted()
	if err != nil {
		return err
	}
	for _, path := range args {
		if err := verifyDocument(path, keys, stdout); err != nil {
			return err
		}
	}
	return nil
}

func verifyDocument(path string, keys signing.Keyring, stdout io.Writer) error {
	document, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	signatures, err := os.ReadFile(path + ".sig")
	if err != nil {
		return err
	}
	id, err := keys.Verify(document, signatures)
	if err != nil {
		return fmt.Errorf("%s: %w (trusted keys in this %s build: %v)", path, err, signing.Channel, keys.IDs())
	}
	fmt.Fprintf(stdout, "%s verified by %s\n", path, id)
	return nil
}
