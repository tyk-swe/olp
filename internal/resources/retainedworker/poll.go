package retainedworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	maxObservationBytes = 4 << 20
	maxDrainBytes       = 64 << 10
	pollHeaderTimeout   = 2 * time.Minute
)

type pollTarget struct {
	config connectors.Config
	model  string
	secret []byte
	client *http.Client
}

func (w *Worker) poll(ctx context.Context, settlements settlementStore, c resources.RetainedClaim) string {
	if settlements == nil {
		return "claim_unavailable"
	}
	closed, err := settlements.CloseUndispatched(ctx, c)
	if err != nil {
		return "claim_unavailable"
	}
	if closed {
		return ""
	}
	r, err := settlements.Resource(ctx, c)
	if err != nil {
		return "resource_unavailable"
	}
	target, errClass := w.resolvePollTarget(ctx, r, c.Operation)
	if errClass != "" {
		return errClass
	}
	upstream, adopt, errClass := pollUpstream(r, c.Hint)
	if errClass != "" {
		return errClass
	}
	resourcePath, endpoint, err := w.path(ctx, r, upstream)
	if err != nil {
		return "unsupported_observation"
	}
	req, errClass := w.buildPollRequest(ctx, target, resourcePath)
	if errClass != "" {
		return errClass
	}
	response, err := target.client.Do(req)
	if err != nil {
		return "poll_failed"
	}
	defer drainAndClose(response)
	if response.StatusCode == http.StatusNotFound && !adopt {
		return w.handleMissingRemote(ctx, settlements, c, r)
	}
	return w.observePollResponse(ctx, settlements, c, r, endpoint, upstream, adopt, response)
}

func (w *Worker) resolvePollTarget(ctx context.Context, r *resources.Resource, operation string) (pollTarget, string) {
	if w.Resolver == nil {
		return pollTarget{}, "credential_or_pin_unavailable"
	}
	pin, err := w.Resolver.ResolveCurrent(ctx, r, operation)
	if err != nil {
		return pollTarget{}, "credential_or_pin_unavailable"
	}
	if w.Egress == nil || w.Auth == nil {
		return pollTarget{}, "connection_unavailable"
	}
	provider, slot := pin.Invocation.Provider, pin.Invocation.Slot
	config := provider.Connector()
	if !w.credentialsEligible(provider, slot, config) {
		return pollTarget{}, "credential_unavailable"
	}
	resolver := providerinvoke.Resolver{
		Credentials:   w.Credentials,
		Policy:        w.Egress,
		Connections:   w.Connections,
		HeaderTimeout: pollHeaderTimeout,
		Scope:         provider.ID + "/" + provider.RevisionID + "/" + slot.ID,
	}
	resolved, err := resolver.Bind(nil, pin.Invocation, nil).Resolve(ctx)
	if err != nil {
		var failure *providerinvoke.ResolutionError
		if errors.As(err, &failure) && !failure.Connection {
			return pollTarget{}, "credential_unavailable"
		}
		return pollTarget{}, "connection_unavailable"
	}
	return pollTarget{
		config: config,
		model:  pin.Invocation.Model,
		secret: resolved.Credential.Secret,
		client: resolved.Client,
	}, ""
}

func (w *Worker) credentialsEligible(provider routing.Provider, slot routing.Slot, config connectors.Config) bool {
	if slot.CredentialID == nil {
		if connectors.SecretRequired(config.AuthMode) {
			return false
		}
	} else if w.Credentials == nil || w.Credentials.Eligibility(*slot.CredentialID) != routing.Eligible {
		return false
	}
	if provider.Network != nil && provider.Network.CredentialID != "" {
		return w.Credentials != nil && w.Credentials.Eligibility(provider.Network.CredentialID) == routing.Eligible
	}
	return true
}

func pollUpstream(r *resources.Resource, hint string) (string, bool, string) {
	if !strings.HasPrefix(r.UpstreamID, "pending:") {
		return r.UpstreamID, false, ""
	}
	if hint == "" {
		return "", false, "submission_unknown"
	}
	return hint, true, ""
}

func (w *Worker) buildPollRequest(ctx context.Context, target pollTarget, resourcePath string) (*http.Request, string) {
	endpoint, err := target.config.ResourceURL(target.model, resourcePath, nil)
	if err != nil {
		return nil, "endpoint_unavailable"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "invalid_endpoint"
	}
	if _, err = w.Auth.Apply(ctx, req, target.config, target.secret, nil); err != nil {
		return nil, "credential_unavailable"
	}
	return req, ""
}

func (w *Worker) handleMissingRemote(ctx context.Context, settlements settlementStore, c resources.RetainedClaim, r *resources.Resource) string {
	if !resources.RetainedDeletionTerminates(r.Kind) {
		return "remote_termination_unverified"
	}
	if settlements == nil {
		return "mutation_evidence_unavailable"
	}
	pendingMutation, err := settlements.HasUnresolvedMutations(ctx, r.UUID)
	if err != nil {
		return "mutation_evidence_unavailable"
	}
	observation, err := missingRemoteObservation(r, c, w.utcNow(), pendingMutation)
	if err != nil {
		return "invalid_expiry_evidence"
	}
	if err := settlements.CommitPoll(ctx, c, r, observation, false); err != nil {
		return "settlement_failed"
	}
	if observation.CoverageComplete {
		return ""
	}
	return "usage_evidence_missing"
}

func missingRemoteObservation(r *resources.Resource, c resources.RetainedClaim, now time.Time, pendingMutation bool) (resources.RetainedObservation, error) {
	canExpire := !pendingMutation &&
		r.State != resources.StateDeleted &&
		(!c.RemoteTerminated || c.TerminalAt != nil && r.ExpiresAt != nil && !c.TerminalAt.Before(*r.ExpiresAt))
	observation, err := resources.ObserveRetainedExpiry(r, now, canExpire)
	if err != nil {
		return resources.RetainedObservation{}, err
	}
	if !observation.RemoteTerminated {
		return resources.RetainedObservation{RemoteTerminated: true, ObservedAt: now}, nil
	}
	return observation, nil
}

func (w *Worker) observePollResponse(ctx context.Context, settlements settlementStore, c resources.RetainedClaim, r *resources.Resource, e retainedapi.Endpoint, upstream string, adopt bool, response *http.Response) string {
	data, document, errClass := decodePollResponse(e, upstream, response)
	if errClass != "" {
		return errClass
	}
	observed, errClass := prepareObservedResource(r, e, upstream, adopt, data)
	if errClass != "" {
		return errClass
	}
	now := w.utcNow()
	if err := resources.EnrichRetained(&observed, e, document, now); err != nil {
		return "invalid_observation"
	}
	resStore := w.resourceClient()
	if resStore == nil {
		return "model_discovery_failed"
	}
	if err := resStore.DiscoverRetainedModels(ctx, &observed, document); err != nil {
		return "model_discovery_failed"
	}
	observation, err := resources.ObserveRetained(&observed, e, document, now)
	if err != nil {
		return "invalid_evidence"
	}
	if settlements == nil {
		return "settlement_failed"
	}
	if err := settlements.CommitPoll(ctx, c, &observed, observation, adopt); err != nil {
		return "settlement_failed"
	}
	return ""
}

func decodePollResponse(e retainedapi.Endpoint, upstream string, response *http.Response) ([]byte, oif.Document, string) {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, oif.Document{}, "poll_failed"
	}
	if response.Body == nil {
		return nil, oif.Document{}, "invalid_observation"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxObservationBytes+1))
	if err != nil || len(data) > maxObservationBytes {
		return nil, oif.Document{}, "invalid_observation"
	}
	document, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxObservationBytes})
	if err != nil {
		return nil, oif.Document{}, "invalid_observation"
	}
	identityField, present := document.Root().Lookup(e.Identity)
	identity, validIdentity := identityField.Text()
	if !present || !validIdentity || identity != upstream {
		return nil, oif.Document{}, "resource_identity_mismatch"
	}
	return data, document, ""
}

func prepareObservedResource(r *resources.Resource, e retainedapi.Endpoint, upstream string, adopt bool, data []byte) (resources.Resource, string) {
	candidate := copyResource(r)
	if !adopt {
		return candidate, ""
	}
	adopted, ok := correlateAdoption(candidate, e, upstream, data)
	if !ok {
		return resources.Resource{}, "uncorrelated_resource"
	}
	return adopted, ""
}

func correlateAdoption(r resources.Resource, e retainedapi.Endpoint, upstream string, data []byte) (resources.Resource, bool) {
	var native struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		Metadata map[string]string `json:"metadata"`
		Labels   map[string]string `json:"labels"`
	}
	if json.Unmarshal(data, &native) != nil {
		return resources.Resource{}, false
	}
	if adoptionMarker(r.Kind, native.Metadata, native.Labels) != r.UUID.String() {
		return resources.Resource{}, false
	}
	if nativeIdentity(e.Identity, native.ID, native.Name) != upstream {
		return resources.Resource{}, false
	}
	adopted := r
	adopted.UpstreamID = upstream
	adopted.State = "active"
	return adopted, true
}

func adoptionMarker(kind string, metadata, labels map[string]string) string {
	if kind == "vertex_tuning_job" {
		return labels[resources.RetainedSubmissionMarkerField]
	}
	return metadata[resources.RetainedSubmissionMarkerField]
}

func nativeIdentity(identityField, id, name string) string {
	if identityField == "name" {
		return name
	}
	return id
}

func copyResource(r *resources.Resource) resources.Resource {
	copied := *r
	copied.Metadata = slices.Clone(r.Metadata)
	if r.ExpiresAt != nil {
		expires := *r.ExpiresAt
		copied.ExpiresAt = &expires
	}
	return copied
}

func drainAndClose(response *http.Response) {
	if response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDrainBytes))
	_ = response.Body.Close()
}
