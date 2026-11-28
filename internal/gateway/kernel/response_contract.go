package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/resources"
)

const responseContractVersion = "native-responses-v2"

type StoredResponseContract struct {
	Version            string      `json:"version"`
	Receipt            oif.Receipt `json:"receipt"`
	Binding            string      `json:"binding"`
	PreviousUpstreamID string      `json:"previous_upstream_id,omitempty"`
}

type ResponseProjection struct {
	UpstreamID, LocalID, Route      string
	PreviousUpstream, PreviousLocal string
}

// NativeResponse owns the validated provider result independently of the local
// identity overlays. Native completion and accounting survive a later binding
// or projection refusal; the immutable source stays available for both.
type NativeResponse struct {
	source     oif.Document
	value      oif.Value
	pointer    string
	upstreamID string
}

func (x *Call) BindResponseContract(contract *StoredResponseContract) {
	if contract != nil {
		x.generation.responseContract = contract
	}
}

func (x *Call) ResponseContract() *StoredResponseContract {
	return x.generation.responseContract
}

func ValidateNativeResponse(source oif.Document, pointer, expectedID string) (NativeResponse, error) {
	value, present := source.Lookup(pointer)
	if !present || value.Kind() != oif.Object {
		return NativeResponse{}, resources.ErrContract
	}
	id, present := value.Lookup("id")
	upstream, valid := id.Text()
	if !present || !valid || upstream == "" || upstream != expectedID {
		return NativeResponse{}, resources.ErrContract
	}
	if object, present := value.Lookup("object"); present {
		kind, valid := object.Text()
		if !valid || kind != "response" {
			return NativeResponse{}, resources.ErrContract
		}
	}
	if state, present := value.Lookup("status"); present {
		status, valid := state.Text()
		if !valid || !wire.ValidResponseStatus(status) {
			return NativeResponse{}, resources.ErrContract
		}
		if status == "completed" {
			output, present := value.Lookup("output")
			if !present || output.Kind() != oif.Array {
				return NativeResponse{}, resources.ErrContract
			}
		}
	}
	if output, present := value.Lookup("output"); present && output.Kind() != oif.Array {
		return NativeResponse{}, resources.ErrContract
	}
	return NativeResponse{source: source, value: value, pointer: pointer, upstreamID: upstream}, nil
}

func (r NativeResponse) Project(p ResponseProjection) (oif.Document, error) {
	if p.UpstreamID != r.upstreamID {
		return oif.Document{}, resources.ErrContract
	}
	value, pointer := r.value, r.pointer
	changes := []oif.Change{{Pointer: pointer + "/id", Value: QuotedResponseID(p.LocalID), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"}}
	if _, present := value.Lookup("model"); present {
		changes = append(changes, oif.Change{Pointer: pointer + "/model", Value: QuotedResponseID(p.Route), Origin: oif.IdentityBinding, Reason: "published response model"})
	}
	if previous, present := value.Lookup("previous_response_id"); present && previous.Kind() != oif.Null {
		upstream, valid := previous.Text()
		if !valid || p.PreviousLocal == "" || upstream != p.PreviousUpstream {
			return oif.Document{}, resources.ErrContract
		}
		changes = append(changes, oif.Change{Pointer: pointer + "/previous_response_id", Value: QuotedResponseID(p.PreviousLocal), Origin: oif.ResourceBinding, Reason: "owner-scoped prior response"})
	}
	projected, err := oif.Apply(r.source, changes)
	if err != nil {
		return oif.Document{}, &responseLocalRefusal{cause: err}
	}
	return projected, nil
}

func (p ResponseProjection) project(source oif.Document, pointer string) (oif.Document, error) {
	native, err := ValidateNativeResponse(source, pointer, p.UpstreamID)
	if err != nil {
		return oif.Document{}, err
	}
	return native.Project(p)
}

func QuotedResponseID(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func ResponseParentProjection(res *resources.Resource, contract *StoredResponseContract) (string, string, error) {
	if res.ParentID == nil {
		return "", "", nil
	}
	if contract == nil || contract.PreviousUpstreamID == "" {
		return "", "", resources.ErrContract
	}
	return contract.PreviousUpstreamID, resources.LocalID(resources.KindStrictResponse, *res.ParentID), nil
}

func responseResourceKind(x *Call) string {
	if x.Strict() {
		return resources.KindStrictResponse
	}
	return resources.KindResponse
}

func responseNativeIdentity(x *Call, fact *events.AttemptFact, upstream string) resources.NativeIdentity {
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	return resources.NativeIdentity{Kind: responseResourceKind(x), APIKeyID: x.keyID,
		RouteSlug: x.Route.Slug, ProviderID: fact.ProviderID, ProviderRevisionID: fact.ProviderRevisionID,
		RouteRevisionID: x.Route.RevisionID, TargetID: fact.TargetID, SlotID: fact.SlotID,
		CredentialID: credential, UpstreamID: upstream}
}
func (s *Server) ReadResponseResource(ctx context.Context, owner, id string) (*resources.Resource, *StoredResponseContract, error) {
	if !strings.HasPrefix(id, resources.KindStrictResponse+"_") {
		r, err := s.Deps.Resources.Get(ctx, resources.KindResponse, owner, id)
		return r, nil, err
	}
	r, payload, err := s.Deps.Resources.ReadContract(ctx, resources.KindStrictResponse, owner, id)
	if err != nil {
		return nil, nil, err
	}
	var contract StoredResponseContract
	if json.Unmarshal(payload, &contract) != nil || contract.Version != responseContractVersion || contract.Binding == "" || contract.Receipt.Obligations.Continuation != "native_response_resource" {
		return nil, nil, resources.ErrContract
	}
	return r, &contract, nil
}
func (s *Server) putStrictResponse(ctx context.Context, x *Call, fact *events.AttemptFact, upstream, state string, metadata []byte, providerExpiry *time.Time) (*resources.Resource, error) {
	provider, ok := x.Snapshot().FindProvider(fact.ProviderID)
	if !ok {
		return nil, resources.ErrContract
	}
	prepared, err := x.generationRequest(&provider, fact.TargetID, fact.UpstreamModel)
	if err != nil || prepared.plan == nil || prepared.plan.Obligations().Continuation != "native_response_resource" {
		return nil, resources.ErrContract
	}
	existing, err := s.Deps.Resources.GetByUpstream(ctx, responseNativeIdentity(x, fact, upstream))
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, resources.ErrNotFound) {
		return nil, err
	}
	expires := s.Now().Add(resources.ContinuationLifetime - time.Second)
	if providerExpiry != nil && providerExpiry.Before(expires) {
		expires = *providerExpiry
	}
	version := responseContractVersion
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	// Retrieval projects only the response and prior-response identities. Keep
	// that binding, not the caller's input or native end-user identifiers.
	previousID := ""
	if previous, present := x.generation.parsed.OIF().Document().Lookup("/previous_response_id"); present && previous.Kind() != oif.Null {
		var valid bool
		previousID, valid = previous.Text()
		if !valid || previousID == "" {
			return nil, resources.ErrContract
		}
	}
	payload, err := json.Marshal(StoredResponseContract{Version: version, Receipt: prepared.plan.Receipt(), Binding: fact.UpstreamModel, PreviousUpstreamID: previousID})
	if err != nil {
		return nil, err
	}
	// Keep only the non-secret serving index and the content-free deferred
	// accounting template. Provider identity bindings remain encrypted.
	var billing map[string]json.RawMessage
	if json.Unmarshal(metadata, &billing) != nil {
		return nil, resources.ErrContract
	}
	metadata, _ = json.Marshal(billing)
	r := &resources.Resource{Kind: resources.KindStrictResponse, APIKeyID: x.keyID, RouteSlug: x.Route.Slug, ProviderID: fact.ProviderID, ProviderRevisionID: fact.ProviderRevisionID, RouteRevisionID: x.Route.RevisionID, TargetID: fact.TargetID, SlotID: fact.SlotID, CredentialID: credential, UpstreamID: upstream, State: state, Metadata: metadata, ExpiresAt: &expires, ContractVersion: &version}
	if previous := x.previousResponse(); previous != nil {
		r.ParentID = &previous.UUID
	}
	return s.Deps.Resources.PutContract(ctx, r, payload)
}
