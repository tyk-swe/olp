package routing

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
)

func codePaidProvider(kind connectors.Kind, profile string) Provider {
	return Provider{
		ID: uuid.NewString(), RevisionID: uuid.NewString(), Name: "p", Kind: kind, Enabled: true,
		Endpoint: "https://example.test/v1", AuthMode: "api_key",
		ProfileID: profile, ProfileRevision: connectors.ProfileRevision,
		Capabilities: []Capability{
			{Model: "m", Operation: "generation", Surface: "anthropic", Mode: "unary"},
			{Model: "m", Operation: "generation", Surface: "anthropic", Mode: "streaming"},
		},
	}
}

func TestCodePaidProtocolRefusesEnvelopedMessages(t *testing.T) {
	t.Parallel()
	if protocol, ok := CodePaidProtocol(codePaidProvider(connectors.KindAnthropic, "anthropic-messages"), "m"); !ok || protocol != codemode.ProtocolMessages {
		t.Fatalf("direct anthropic provider: protocol=%q ok=%v", protocol, ok)
	}
	for _, tc := range []struct {
		kind    connectors.Kind
		profile string
	}{
		{connectors.KindVertexAI, "vertex-anthropic"},
		{connectors.KindBedrock, "bedrock-anthropic-invoke"},
	} {
		if protocol, ok := CodePaidProtocol(codePaidProvider(tc.kind, tc.profile), "m"); ok {
			t.Errorf("%s envelops messages but was offered as %q", tc.profile, protocol)
		}
	}
}
