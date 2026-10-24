// Package retainedworker reconciles provider-retained paid work. It never
// repeats a paid mutation, estimates a charge from snapshots, or drops liability.
package retainedworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

type Worker struct {
	Store       *resources.Store
	Resolver    *resources.Resolver
	Credentials runtime.Credentials
	Auth        *connectors.Auth
	Egress      *egress.Policy
	Connections *egress.ConnectionClientCache
	Log         *slog.Logger
}

func (w *Worker) Run(ctx context.Context) {
	if w.Connections == nil {
		w.Connections = egress.NewConnectionClientCache(128)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		err := w.Pass(ctx)
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			if ctx.Err() == nil && w.Log != nil {
				w.Log.Warn("retained reconciliation failed", "error", err)
			}
		}
		_ = w.Store.CheckpointRetained(ctx, outcome, false)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (w *Worker) Pass(ctx context.Context) error {
	if w.Connections == nil {
		w.Connections = egress.NewConnectionClientCache(128)
	}
	claims, err := w.Store.ClaimRetained(ctx, 16)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, claim := range claims {
		wg.Go(func() {
			work, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				t := time.NewTicker(30 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-work.Done():
						return
					case <-t.C:
						_ = w.Store.CheckpointRetained(work, usage.OutcomeSuccess, false)
						if w.Store.HeartbeatRetained(work, claim) != nil {
							cancel()
							return
						}
					}
				}
			}()
			class := ""
			select {
			case sem <- struct{}{}:
				class = w.poll(work, claim)
				<-sem
			case <-work.Done():
				class = "poll_cancelled"
			}
			cancel()
			<-done
			finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer stop()
			_ = w.Store.FinishRetainedClaim(finish, claim, class)
		})
	}
	wg.Wait()
	return nil
}
func (w *Worker) poll(ctx context.Context, c resources.RetainedClaim) string {
	closed, err := w.Store.CloseUndispatchedRetained(ctx, c)
	if err != nil {
		return "claim_unavailable"
	}
	if closed {
		return ""
	}
	r, err := w.Store.RetainedClaimResource(ctx, c)
	if err != nil {
		return "resource_unavailable"
	}
	provider, route, slot, err := w.Resolver.ResolveCurrent(ctx, r, c.Operation)
	if err != nil {
		return "credential_or_pin_unavailable"
	}
	if w.Egress == nil || w.Auth == nil {
		return "connection_unavailable"
	}
	config := provider.Connector()
	var secret, networkSecret []byte
	if slot.CredentialID != nil {
		if w.Credentials == nil || w.Credentials.Eligibility(*slot.CredentialID) != routing.Eligible {
			return "credential_unavailable"
		}
		secret, _, err = w.Credentials.Secret(ctx, nil, *slot.CredentialID)
		if err != nil {
			return "credential_unavailable"
		}
	} else if connectors.SecretRequired(config.AuthMode) {
		return "credential_unavailable"
	}
	if provider.Network != nil && provider.Network.CredentialID != "" {
		if w.Credentials == nil || w.Credentials.Eligibility(provider.Network.CredentialID) != routing.Eligible {
			return "credential_unavailable"
		}
		networkSecret, err = w.Credentials.NetworkSecret(ctx, nil, provider.ID, provider.Network.CredentialID)
		if err != nil {
			return "credential_unavailable"
		}
	}
	client, err := w.Connections.ClientScoped(provider.ID+"/"+provider.RevisionID+"/"+slot.ID, *w.Egress, provider.Network, networkSecret, 2*time.Minute)
	if err != nil {
		return "connection_unavailable"
	}
	model := ""
	for _, target := range route.Targets {
		if target.ID == r.TargetID {
			model = target.ProviderModel
		}
	}
	if model == "" {
		return "pin_unavailable"
	}
	upstream := r.UpstreamID
	adopt := strings.HasPrefix(upstream, "pending:")
	if adopt {
		if c.Hint == "" {
			return "submission_unknown"
		}
		upstream = c.Hint
	}
	path, e, err := w.path(ctx, r, upstream, c.Operation)
	if err != nil {
		return "unsupported_observation"
	}
	endpoint, err := config.ResourceURL(model, path, nil)
	if err != nil {
		return "endpoint_unavailable"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "invalid_endpoint"
	}
	if _, err = w.Auth.Apply(ctx, req, config, secret, nil); err != nil {
		return "credential_unavailable"
	}
	response, err := client.Do(req)
	if err != nil {
		return "poll_failed"
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && !adopt {
		// Missing asynchronous jobs do not prove execution has stopped.
		if !resources.RetainedDeletionTerminates(r.Kind) {
			return "remote_termination_unverified"
		}
		// Storage disappearance verifies termination, never its usage.
		pendingMutation, mutationErr := w.Store.HasUnresolvedRetainedMutations(ctx, r.UUID)
		if mutationErr != nil {
			return "mutation_evidence_unavailable"
		}
		canExpire := !pendingMutation && (!c.RemoteTerminated || c.TerminalAt != nil && r.ExpiresAt != nil && !c.TerminalAt.Before(*r.ExpiresAt))
		observation, expiryErr := resources.ObserveRetainedExpiry(r, time.Now().UTC(), canExpire && r.State != resources.StateDeleted)
		if expiryErr != nil {
			return "invalid_expiry_evidence"
		}
		if !observation.RemoteTerminated {
			observation = resources.RetainedObservation{RemoteTerminated: true, ObservedAt: time.Now().UTC()}
		}
		if err = w.Store.CommitRetainedPoll(ctx, c, r, observation, false); err != nil {
			return "settlement_failed"
		}
		if observation.CoverageComplete {
			return ""
		}
		return "usage_evidence_missing"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "poll_failed"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return "invalid_observation"
	}
	document, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 4 << 20})
	if err != nil {
		return "invalid_observation"
	}
	identityField, present := document.Root().Lookup(e.Identity)
	identity, validIdentity := identityField.Text()
	if !present || !validIdentity || identity != upstream {
		return "resource_identity_mismatch"
	}
	if adopt {
		var native struct {
			ID       string            `json:"id"`
			Name     string            `json:"name"`
			Metadata map[string]string `json:"metadata"`
			Labels   map[string]string `json:"labels"`
		}
		if json.Unmarshal(data, &native) != nil {
			return "uncorrelated_resource"
		}
		marker := native.Metadata["olp_submission_id"]
		if r.Kind == "vertex_tuning_job" {
			marker = native.Labels["olp_submission_id"]
		}
		if marker != r.UUID.String() {
			return "uncorrelated_resource"
		}
		identity := native.ID
		if e.Identity == "name" {
			identity = native.Name
		}
		if identity != upstream {
			return "uncorrelated_resource"
		}
		r.UpstreamID = upstream
		r.State = "active"
	}
	if err = resources.EnrichRetained(r, e, document, time.Now().UTC()); err != nil {
		return "invalid_observation"
	}
	if err = w.Store.DiscoverRetainedModels(ctx, r, document); err != nil {
		return "model_discovery_failed"
	}
	observation, err := resources.ObserveRetained(r, e, document, time.Now().UTC())
	if err != nil {
		return "invalid_evidence"
	}
	if err = w.Store.CommitRetainedPoll(ctx, c, r, observation, adopt); err != nil {
		return "settlement_failed"
	}
	return ""
}
func (w *Worker) path(ctx context.Context, r *resources.Resource, upstream, operation string) (string, retainedapi.Endpoint, error) {
	var family retainedapi.Family
	for _, f := range retainedapi.Families() {
		if f.Kind == r.Kind {
			family = f
			break
		}
		for _, child := range f.Children {
			if child.Kind == r.Kind {
				family = child
				family.Operation = f.Operation
				family.Surface = f.Surface
			}
		}
	}
	e := retainedapi.Endpoint{Family: family, Action: "get", Method: http.MethodGet}
	if family.Kind == "" {
		return "", e, errors.New("unknown retained family")
	}
	path := ""
	switch r.Kind {
	case "fine_tuning_job":
		path = "fine_tuning/jobs/" + url.PathEscape(upstream)
	case "vertex_tuning_job":
		parts := strings.Split(upstream, "/tuningJobs/")
		path = "tuningJobs/" + url.PathEscape(parts[len(parts)-1])
	case "vector_store":
		path = "vector_stores/" + url.PathEscape(upstream)
	case "container":
		path = "containers/" + url.PathEscape(upstream)
	case "context_cache":
		path = "cachedContents/" + url.PathEscape(strings.TrimPrefix(upstream, "cachedContents/"))
	case "eval_run", "vector_store_file", "vector_store_batch", "container_file":
		if r.ParentID == nil {
			return "", e, errors.New("missing parent")
		}
		parent, err := w.Store.GetByUUID(ctx, *r.ParentID, r.APIKeyID)
		if err != nil {
			return "", e, err
		}
		prefix := "vector_stores"
		child := "files"
		if r.Kind == "eval_run" {
			prefix = "evals"
			child = "runs"
		}
		if r.Kind == "vector_store_batch" {
			child = "file_batches"
		}
		if r.Kind == "container_file" {
			prefix = "containers"
		}
		path = prefix + "/" + url.PathEscape(parent.UpstreamID) + "/" + child + "/" + url.PathEscape(upstream)
	default:
		return "", e, errors.New("unqualified retained observation")
	}
	return path, e, nil
}
