package gateway

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type endUserRuntime struct{ *fakeRuntime }

func (f endUserRuntime) EndUserDigest(project *string, identifier string) string {
	return access.DigestEndUser(secrets.NewAuthKey([]byte(strings.Repeat("k", 32)), "installation"), project, identifier)
}

func endUserHarness(t *testing.T, source string) *harness {
	t.Helper()
	h := strictHarness(t, nil)
	identifyHarness(h, source)
	return h
}

func identifyHarness(h *harness, source string) {
	authority := h.rt.keys[fullKey]
	authority.Policy.EndUserSource = &source
	h.rt.keys[fullKey] = authority
	h.gateway.Runtime = endUserRuntime{h.rt}
}

func assertEndUserEnvelope(t *testing.T, envelope Envelope, digest, identifier string) {
	t.Helper()
	if envelope.EndUserDigest != digest {
		t.Fatalf("terminal identity = %q, want %q", envelope.EndUserDigest, digest)
	}
	// Lists and pre-route refusals have terminal metadata but no billing event.
	if envelope.Route == "" {
		return
	}
	event := accountingEvent(envelope)
	if event == nil {
		t.Fatal("identified operation lost its accounting event")
	}
	if _, err := usage.Validate(event); err != nil {
		t.Fatalf("invalid accounting event: %v", err)
	}
	encoded, err := usage.Encode(event)
	if err != nil || bytes.Contains(encoded, []byte(identifier)) {
		t.Fatalf("identified terminal is invalid or contains the raw identifier: %v", err)
	}
}
