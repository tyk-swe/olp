package openaistate

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestGeminiManagedFileRejectsOversizedProviderLine(t *testing.T) {
	t.Parallel()
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	p := newPinnedProvider(t, spool, `{"key":"huge","error":{"message":"`+strings.Repeat("a", managedbatch.LineLimit+2)+`"}}`+"\n")
	res := &resources.Resource{UpstreamID: "files/result", RouteSlug: "route", Metadata: json.RawMessage(`{"role":"error"}`)}
	failure, attempt := p.onlyAttempt(t, func(ctx context.Context, w http.ResponseWriter, x *kernel.Call) *fault.Error {
		return p.surface.geminiManagedFileContent(ctx, w, x, res, p.pin)
	})
	if failure.Code != "fidelity_protocol_violation" {
		t.Fatalf("unexpected download result: %+v", failure)
	}
	if attempt.Class != kernel.ClassProtocol || attempt.LocalRefusal {
		t.Fatalf("oversized native line was not blamed on the provider: %+v", attempt)
	}
}
