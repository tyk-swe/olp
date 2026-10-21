package batches

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestSettlementLeaseHasBoundedLifetime(t *testing.T) {
	worker := &Worker{}
	ctx, stop := worker.keepLease(context.Background(), Settlement{})
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > reconcileTimeout {
		t.Fatalf("settlement could renew forever without a read deadline: %v %v", deadline, ok)
	}
}

func TestSettlementContextInterruptsStalledResponseBody(t *testing.T) {
	headers := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headers)
		<-r.Context().Done()
	}))
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	reader := &OpenAIReader{Auth: connectors.NewAuth(policy), Egress: policy}
	target := &PinnedTarget{Config: connectors.Config{Kind: "openai", AuthMode: "api_key", Endpoint: server.URL + "/v1"}, Secret: []byte("test-key"), Client: server.Client()}
	parent, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	ctx, stop := (&Worker{}).keepLease(parent, Settlement{})
	defer stop()
	job := "job"
	start := time.Now()
	if _, err := reader.Poll(ctx, Settlement{UpstreamID: &job}, target); err == nil || time.Since(start) > time.Second {
		t.Fatalf("body read ignored settlement cancellation: %v", err)
	}
	select {
	case <-headers:
	default:
		t.Fatal("fixture did not reach the stalled-body phase")
	}
}
