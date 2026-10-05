package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/signing"
)

const developmentSeed = "../../devkey/" + signing.DevelopmentKeyID + ".seed"

func TestSignThenVerify(t *testing.T) {
	document := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(document, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"sign", "-key-id", signing.DevelopmentKeyID, "-seed-file", developmentSeed, document}, &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", document}, &out); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(document, []byte("{ }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", document}, &out); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("an edited document verified: %v", err)
	}
}

func TestSignRefusesAMismatchedSeed(t *testing.T) {
	document := filepath.Join(t.TempDir(), "catalog.json")
	_ = os.WriteFile(document, []byte("{}\n"), 0o644)
	t.Setenv("OLP_SIGNING_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err := run([]string{"sign", "-key-id", signing.DevelopmentKeyID, document}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed with a seed that is not the named key")
	}
	if err := run([]string{"sign", "-key-id", "unknown-key", document}, &bytes.Buffer{}); err == nil {
		t.Fatal("signed with a key the build does not know")
	}
}
