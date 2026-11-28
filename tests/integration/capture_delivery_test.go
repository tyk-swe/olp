//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestDeliveringACaptureIsAuditedUnderItsProject(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	keys, installation := notificationKeys(t, pool)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(receiver.Close)
	sinkID := uuid.New()
	mustExec(t, pool, "INSERT INTO olp.export_sinks(id,name,type,destination,streams,format,etag,created_by) VALUES($1,'captures','https',$2,'{requests}','json',$3,$4)", sinkID, receiver.URL, uuid.New(), f.ownerID)
	mustExec(t, pool, "INSERT INTO olp.capture_policies(id,project_id,sink_id,sample_ratio,include,max_bytes,etag,created_by) VALUES($1,$2,$3,'1','{input}',65536,$4,$5)", uuid.New(), f.projectID, sinkID, uuid.New(), f.ownerID)
	mustExec(t, pool, "UPDATE olp.capture_configuration SET enabled=true")
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	manager := capture.NewManager(pool, keys, installation, destination.NewSender(policy, func(string) string { return "" }, installation), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	requestID := uuid.NewString()
	harness.Eventually(t, "the capture policy to admit a request", 5*time.Second, func() (bool, string) {
		collector := manager.Resolve(f.projectID, "chat", "", "", requestID)
		if collector == nil {
			return false, "no collector"
		}
		collector.CaptureDocument(map[string]json.RawMessage{"messages": json.RawMessage(`[]`)})
		identity := capture.Identity{Route: "chat", ProjectID: f.projectID, Outcome: "success", Mode: capture.Unary}
		return collector.Finish(t.Context(), identity, time.Now()), "capture was not queued"
	})
	harness.Eventually(t, "the delivery to be audited", 5*time.Second, func() (bool, string) {
		var audited int
		err := pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='capture.delivered' AND resource_type='payload_capture' AND resource_id=$1 AND outcome='success' AND project_id=$2 AND actor_user_id IS NULL AND actor_management_token_id IS NULL", requestID, f.projectID).Scan(&audited)
		return err == nil && audited == 1, fmt.Sprintf("%d rows: %v", audited, err)
	})
}
