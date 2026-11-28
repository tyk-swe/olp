package openaistate

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestManagedFileRefreshAttributesMetadataFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, storedMetadata, providerBody string
		local                              bool
	}{
		{"provider byte count", `{"adapter":"mistral"}`, `{"id":"file-native","bytes":-1}`, false},
		{"stored byte count", `{"adapter":"mistral","provider_file":{"bytes":-1}}`, `{"id":"file-native","bytes":10}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPinnedProvider(t, nil, tc.providerBody)
			id := uuid.New()
			res := &resources.Resource{UUID: id, ID: resources.LocalID(resources.KindFile, id), Kind: resources.KindFile, UpstreamID: "file-native", Metadata: json.RawMessage(tc.storedMetadata)}
			failure, attempt := p.onlyAttempt(t, func(ctx context.Context, w http.ResponseWriter, x *kernel.Call) *fault.Error {
				return p.surface.getPinnedFile(ctx, w, x, keyauthority.Authority{}, res, p.pin)
			})
			if failure.Status != http.StatusBadGateway || failure.Code != "fidelity_protocol_violation" {
				t.Fatalf("unexpected file refresh failure: %+v", failure)
			}
			if attempt.Class != kernel.ClassProtocol || attempt.LocalRefusal != tc.local {
				t.Fatalf("unexpected attempt evidence: %+v", attempt)
			}
		})
	}
}
