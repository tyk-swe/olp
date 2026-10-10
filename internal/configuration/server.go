package configuration

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/secretstore"
)

type Server struct {
	Access *access.Server
	Egress *egress.Policy
	// Unconfined is the deployment's unconfined plugin tier, or nil where it
	// enables none, for plugin providers to pin.
	Unconfined *plugins.Unconfined
	// Limiter, when the deployment enforces limits, receives budget balances a
	// document changes before the change publishes.
	Limiter                *limits.Limiter
	VendorKind             func(vendor string) (string, bool)
	StoreNetworkCredential func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
	StoreCredential        func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
}

func (s *Server) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/configuration/export", s.export)
	s.Access.Route(mux, "POST /api/v1/configuration/plan", s.planEndpoint, access.MaxBody(4<<20))
	s.Access.Route(mux, "POST /api/v1/configuration/apply", s.applyEndpoint, access.MaxBody(4<<20), access.Deadline(60*time.Second))
}

func (s *Server) export(r *http.Request, p access.Principal) (access.Reply, error) {
	doc, err := s.exportDocument(r.Context(), s.Access.Pool)
	if err != nil {
		return access.Reply{}, err
	}
	if doc.SAML != nil || len(doc.WorkloadIssuers) > 0 || len(doc.SCIMGroupMappings) > 0 {
		if err := p.Authorize(access.Access); err != nil {
			return access.Reply{}, err
		}
	}
	digest, err := Digest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	doc.ExportedAt = time.Now().UTC().Format(time.RFC3339)
	return access.OK(map[string]any{"digest": digest, "document": doc}), nil
}

type promotionInput struct {
	Document         *Document                        `json:"document"`
	ExpectedDigest   *string                          `json:"expected_digest"`
	SecretBindings   map[string]string                `json:"secret_bindings"`
	ExternalBindings map[string]secretstore.Reference `json:"external_credential_bindings"`
}

func (s *Server) planEndpoint(r *http.Request, p access.Principal) (access.Reply, error) {
	var input promotionInput
	if err := access.DecodeUnique(r, &input, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	if input.Document.SAML != nil || len(input.Document.WorkloadIssuers) > 0 || len(input.Document.SCIMGroupMappings) > 0 {
		if err := p.Authorize(access.Access); err != nil {
			return access.Reply{}, err
		}
	}
	bindings, err := input.bindings()
	if err != nil {
		return access.Reply{}, err
	}
	result, err := s.plan(r.Context(), s.Access.Pool, input.Document, bindings, input.ExpectedDigest)
	if err != nil {
		return access.Reply{}, err
	}
	if err = authorizeSinkPromotion(p, input.Document); err != nil {
		return access.Reply{}, err
	}
	if err = authorizeCatalogPublication(r.Context(), s.Access.Pool, p, input.Document); err != nil {
		return access.Reply{}, err
	}
	return access.OK(result), nil
}

func (s *Server) applyEndpoint(r *http.Request, initial access.Principal) (access.Reply, error) {
	var input promotionInput
	if err := access.DecodeUnique(r, &input, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	bindings, err := input.bindings()
	if err != nil {
		return access.Reply{}, err
	}
	if err := s.prepareWorkloadIssuers(r.Context(), initial, input.Document); err != nil {
		return access.Reply{}, err
	}
	preparedMCP, err := s.prepareMCPServers(r.Context(), initial, input.Document, bindings)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if input.Document.SAML != nil || len(input.Document.WorkloadIssuers) > 0 || len(input.Document.SCIMGroupMappings) > 0 {
		if err := p.Authorize(access.Access); err != nil {
			return access.Reply{}, err
		}
	}
	doc := input.Document
	doc.canonicalize()
	digest, err := Digest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, bindingFingerprint(doc, digest, bindings, input.ExpectedDigest))
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	result, err := s.plan(r.Context(), tx, doc, bindings, input.ExpectedDigest)
	if err != nil {
		return access.Reply{}, err
	}
	if len(result.Conflicts) > 0 || len(result.Blockers) > 0 {
		body := map[string]any{
			"type":      "https://openllmproxy.dev/problems/configuration_not_applicable",
			"title":     http.StatusText(409),
			"status":    409,
			"detail":    "The artifact cannot be applied while conflicts or blockers remain.",
			"digest":    result.Digest,
			"actions":   result.Actions,
			"conflicts": result.Conflicts,
			"blockers":  result.Blockers,
		}
		return access.Reply{Status: 409, Body: body}, nil
	}
	if doc.Pricing != nil {
		changed, err := s.pricingChanged(r.Context(), tx, doc)
		if err != nil {
			return access.Reply{}, err
		}
		// Changing pricing is an installation setting as well.
		if changed {
			if err := p.Authorize(access.Settings); err != nil {
				return access.Reply{}, err
			}
		}
	}
	if err = s.applyDocument(r.Context(), tx, p, doc, bindings); err != nil {
		return access.Reply{}, err
	}
	if err = s.applyMCPServers(r.Context(), tx, p, doc, bindings, preparedMCP); err != nil {
		return access.Reply{}, err
	}
	if err := s.applySAMLDefinition(r, tx, p, input.Document.SAML); err != nil {
		return access.Reply{}, err
	}

	if err = access.Audit(r.Context(), tx, r, p.Actor(), "configuration.apply", "configuration", digest, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.OK(result)
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
