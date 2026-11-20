package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) responseCall(w http.ResponseWriter, r *http.Request, use RetainedUse, op func(context.Context, *Call, keyauthority.Authority, *resources.Resource, *Pin, *routing.Route) *fault.Error) {
	x, authority, done := s.StateBegin(w, r, wire.Responses)
	if done {
		return
	}
	defer s.Gate.Release(r.Context())
	if !authority.Policy.AllowProviderState {
		s.Refuse(x, w, providerStateForbidden())
		return
	}
	res, contract, err := s.readResponseResource(r.Context(), authority.ID, r.PathValue("id"))
	if errors.Is(err, resources.ErrNotFound) {
		s.Refuse(x, w, fault.NotFoundError("not_found", "No stored response with this identifier exists for this key."))
		return
	}
	if err != nil {
		s.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored response could not be read."))
		return
	}
	p, route, e := s.admitStoredResponse(r.Context(), x, authority, res, contract, use)
	if e != nil {
		s.Refuse(x, w, e)
		return
	}
	if contract != nil {
		x.generation.responseContract = contract
	}
	x.Route = route
	ctx, cancel := s.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.ResourceSettle(ctx, x, p)
	if e := s.ReserveStateAndSlot(ctx, x, authority, p, route); e != nil {
		s.Refuse(x, w, e)
		return
	}
	if e := op(ctx, x, authority, res, p, route); e != nil {
		s.Refuse(x, w, e)
	}
}

func (s *Server) responseUpstream(ctx context.Context, x *Call, res *resources.Resource, p *Pin, method, suffix string, body []byte, w http.ResponseWriter, query url.Values) *fault.Error {
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, responsePath(p.Provider.Connector(), suffix), query)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	resp, failure := s.PinnedDo(ctx, x, p, method, endpoint, body, "application/json")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()

	result, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
	}
	doc, parseErr := oif.ParseJSON(result, oif.Limits{MaxBytes: int(s.Config.MaxResponseBytes)})
	if parseErr != nil {
		if res.Kind == resources.KindStrictResponse {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider returned an invalid native response.")
		}
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	var native nativeResponse
	if res.Kind == resources.KindStrictResponse {
		native, err = validateNativeResponse(doc, "", res.UpstreamID)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider returned an invalid native response identity or grammar.")
		}
	} else {
		id, present := doc.Root().Lookup("id")
		upstream, valid := id.Text()
		if !present || !valid || upstream != res.UpstreamID {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an unbound response identity.")
		}
	}
	observation, err := openai.ObserveResponseDocumentUsage(doc)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned malformed response usage.")
	}
	if observation.Status == "completed" {
		output, present := doc.Root().Lookup("output")
		if !present || output.Kind() != oif.Array {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned an invalid completed response output.")
		}
	}
	resp.Exchange.Complete(ClassSuccess)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	if e := s.applyResponseObservation(commitCtx, res.ID, observation); e != nil {
		return e
	}
	nativeStatus := observation.Status
	var out []byte
	if res.Kind == resources.KindStrictResponse {
		projection := responseProjection{upstreamID: res.UpstreamID, localID: res.ID, route: res.RouteSlug}
		projection.previousUpstream, projection.previousLocal, err = responseParentProjection(res, x.generation.responseContract)
		if err != nil {
			return fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The retained parent response cannot be reconstructed.")
		}
		strictResultDoc, err := native.project(projection)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The retained provider response could not be projected to its local identities.")
		}
		if nativeStatus == "failed" {
			strictResultDoc, err = fault.RedactNativeFailureDocument(strictResultDoc, x.sensitive)
			if err != nil {
				return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider failure contained unsafe native fields.")
			}
		}
		out = strictResultDoc.Bytes()
	} else {
		var projected oif.Document
		projected, err = oif.Apply(doc, []oif.Change{
			{Pointer: "/id", Value: quotedResponseID(res.ID), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"},
			{Pointer: "/model", Value: quotedResponseID(res.RouteSlug), Origin: oif.IdentityBinding, Reason: "published response model"},
		})
		if err == nil {
			out, err = s.mapPreviousResponse(commitCtx, projected.Bytes(), res.NativeIdentity(), nil)
		}
	}
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed response object.")
	}
	out, err = s.mapGenerationResultReferences(commitCtx, x, res.NativeIdentity(), out)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "resource_affinity", "The retained provider returned an unowned result reference.")
	}
	if method == http.MethodPost && res.Kind == resources.KindStrictResponse {
		current, present := x.request.release.Snapshot().FindRoute(res.RouteSlug)
		if !present || retainedContract(true, current.Fidelity, RetainedRetrieval) != nil || s.checkRouteFidelity(ctx, &current) != nil {
			// Cancellation remains available for cleanup, but cannot return content
			// that the same resource's retrieval guard forbids.
			out, _ = json.Marshal(map[string]string{"id": res.ID, "object": "response", "status": nativeStatus})
		}
	}
	s.WriteStateJSON(w, x, out, resp.Exchange)
	return nil
}

func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		query, stream, e := responseRetrievalQuery(r.URL.RawQuery)
		if e != nil {
			return e
		}
		if stream {
			return s.streamStoredResponse(ctx, w, x, res, p, query)
		}
		return s.responseUpstream(ctx, x, res, p, http.MethodGet, "/"+url.PathEscape(res.UpstreamID), nil, w, query)
	})
}

func (s *Server) cancelResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		return s.responseUpstream(ctx, x, res, p, http.MethodPost, "/"+url.PathEscape(res.UpstreamID)+"/cancel", []byte(`{}`), w, nil)
	})
}

func (s *Server) deleteResponse(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, RetainedHousekeeping, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, responsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), nil)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodDelete, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				commitCtx, stopCommit := ResourceCommitContext(ctx)
				defer stopCommit()
				_ = s.Deps.Resources.Tombstone(commitCtx, res.ID)
				return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		result, _ := resp.ReadBody(s.Config.MaxResponseBytes)
		resp.Exchange.Complete(ClassSuccess)
		// The provider already deleted the response: the mapping must follow
		// even when the client has gone.
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		defer stopCommit()
		if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil {
			return fault.ServerError(http.StatusInternalServerError, "internal_error", "The response mapping could not be deleted.")
		}
		out, err := RewriteID(result, "id", res.ID)
		if err != nil {
			encoded, _ := json.Marshal(res.ID)
			out, _ = json.Marshal(map[string]json.RawMessage{
				"id": encoded, "object": json.RawMessage(`"response"`), "deleted": json.RawMessage(`true`),
			})
		}
		s.WriteStateJSON(w, x, out, resp.Exchange)
		return nil
	})
}

func (s *Server) responseInputItems(w http.ResponseWriter, r *http.Request) {
	s.responseCall(w, r, RetainedRetrieval, func(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, p *Pin, route *routing.Route) *fault.Error {
		query := url.Values{}
		for _, name := range []string{"limit", "after", "order"} {
			if value := r.URL.Query().Get(name); value != "" {
				query.Set(name, value)
			}
		}
		endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, responsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)+"/input_items"), query)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
		}
		resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
		if failure != nil {
			if failure.Status == http.StatusNotFound {
				return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
			}
			return UpstreamError(failure)
		}
		defer resp.Body.Close()

		result, err := resp.ReadBody(s.Config.MaxResponseBytes)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read.")
		}
		document, err := oif.ParseJSON(result, oif.Limits{MaxBytes: int(s.Config.MaxResponseBytes)})
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned invalid input items.")
		}
		refs, err := generationrefs.InInputItems(document)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned input items with unbound resource references.")
		}
		resp.Exchange.Complete(ClassSuccess)
		projected, err := generationrefs.ProjectInputItems(document, refs, s.generationResultResolver(ctx, x, res.NativeIdentity()))
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider returned input items with unbound resource references.")
		}
		s.WriteStateJSON(w, x, projected.Bytes(), resp.Exchange)
		return nil
	})
}
