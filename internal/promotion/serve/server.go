package serve

import (
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublications"
	"github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/mcpentries"
	"github.com/tyk-swe/olp/internal/promotion/retrievalbindings"
	"github.com/tyk-swe/olp/internal/promotion/workloadissuers"
	"github.com/tyk-swe/olp/internal/sinks"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func Register(s *promotion.Server, mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/configuration/export", func(r *http.Request, p access.Principal) (access.Reply, error) {
		return export(s, r, p)
	})
	s.Access.Route(mux, "POST /api/v1/configuration/plan", func(r *http.Request, p access.Principal) (access.Reply, error) {
		return planEndpoint(s, r, p)
	}, access.MaxBody(4<<20))
	s.Access.Route(mux, "POST /api/v1/configuration/apply", func(r *http.Request, p access.Principal) (access.Reply, error) {
		return applyEndpoint(s, r, p)
	}, access.MaxBody(4<<20), access.Deadline(60*time.Second))
}

func export(s *promotion.Server, r *http.Request, p access.Principal) (access.Reply, error) {
	doc, err := exportDocument(s, r.Context(), s.Access.Pool)
	if err != nil {
		return access.Reply{}, err
	}
	if doc.SAML != nil || len(doc.WorkloadIssuers) > 0 || len(doc.SCIMGroupMappings) > 0 {
		if err := p.Authorize(access.Access); err != nil {
			return access.Reply{}, err
		}
	}
	digest, err := promotion.CanonicalizeAndDigest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	doc.ExportedAt = time.Now().UTC().Format(time.RFC3339)
	return access.OK(map[string]any{"digest": digest, "document": doc}), nil
}

type promotionInput struct {
	CodeBindings     promotion.CodeBindings           `json:"code_bindings"`
	Document         *promotion.Document              `json:"document"`
	ExpectedDigest   *string                          `json:"expected_digest"`
	SecretBindings   map[string]string                `json:"secret_bindings"`
	ExternalBindings map[string]secretstore.Reference `json:"external_credential_bindings"`
}

func planEndpoint(s *promotion.Server, r *http.Request, p access.Principal) (access.Reply, error) {
	var Input promotionInput
	if err := access.DecodeUnique(r, &Input, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if Input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	Input.Document.CodeBindings = Input.CodeBindings
	if Input.Document.SAML != nil || len(Input.Document.WorkloadIssuers) > 0 || len(Input.Document.SCIMGroupMappings) > 0 {
		if err := p.Authorize(access.Access); err != nil {
			return access.Reply{}, err
		}
	}
	if err := Input.authorizeExternalBindings(p); err != nil {
		return access.Reply{}, err
	}
	bindings, err := Input.bindings()
	if err != nil {
		return access.Reply{}, err
	}
	if err := code.AuthorizeCodePromotion(r.Context(), s.Access.Pool, p, Input.Document); err != nil {
		return access.Reply{}, err
	}
	result, err := plan(s, r.Context(), s.Access.Pool, Input.Document, bindings, Input.ExpectedDigest)
	if err != nil {
		return access.Reply{}, err
	}
	if err = sinks.AuthorizeSinkPromotion(p, Input.Document); err != nil {
		return access.Reply{}, err
	}
	if err = modelcatalog.AuthorizeCatalogPublication(r.Context(), s.Access.Pool, p, Input.Document); err != nil {
		return access.Reply{}, err
	}
	return access.OK(result), nil
}

func applyEndpoint(s *promotion.Server, r *http.Request, _ access.Principal) (access.Reply, error) {
	var Input promotionInput
	if err := access.DecodeUnique(r, &Input, 4<<20); err != nil {
		return access.Reply{}, err
	}
	if Input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	bindings, err := Input.bindings()
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = Input.authorizeExternalBindings(p); err != nil {
		return access.Reply{}, err
	}
	if err := authorizeIdentityPromotion(p, Input.Document); err != nil {
		return access.Reply{}, err
	}
	// Normalize the same local metadata used by network preparation before
	// computing its replay fingerprint. Completed writes do not depend on the
	// current availability or catalog of an upstream service.
	if err := workloadissuers.ValidateAndNormalizeWorkloadEntries(Input.Document); err != nil {
		return access.Reply{}, err
	}
	if err := agentpublications.ValidateAgentPublications(s, Input.Document); err != nil {
		return access.Reply{}, err
	}
	if err := mcpentries.ValidateMCPServers(s, Input.Document); err != nil {
		return access.Reply{}, err
	}
	if err := retrievalbindings.ValidateAndNormalizeRetrievalBindings(Input.Document); err != nil {
		return access.Reply{}, err
	}
	Input.Document.CodeBindings = Input.CodeBindings
	if err := code.AuthorizeCodePromotion(r.Context(), tx, p, Input.Document); err != nil {
		return access.Reply{}, err
	}
	doc := Input.Document
	doc.Canonicalize()
	digest, err := promotion.CanonicalizeAndDigest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, bindingFingerprint(doc, digest, bindings, Input.ExpectedDigest))
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	var preparedServers map[[2]string]mcpentries.PreparedMCP
	if len(doc.WorkloadIssuers) > 0 || len(doc.MCPServers) > 0 {
		if err = tx.Rollback(r.Context()); err != nil {
			return access.Reply{}, err
		}
		// Never hold the installation mutation lock during upstream discovery.
		if err = workloadissuers.PrepareWorkloadIssuers(s, r.Context(), p, doc); err != nil {
			return access.Reply{}, err
		}
		preparedServers, err = mcpentries.PrepareMCPServers(s, r.Context(), p, doc, bindings)
		if err != nil {
			return access.Reply{}, err
		}
		tx, p, err = s.Access.BeginMutation(r)
		if err != nil {
			return access.Reply{}, err
		}
		defer tx.Rollback(r.Context())
		if err = Input.authorizeExternalBindings(p); err != nil {
			return access.Reply{}, err
		}
		if err = authorizeIdentityPromotion(p, doc); err != nil {
			return access.Reply{}, err
		}
		claim, replayed, err = s.Access.Replay(r, tx, p, bindingFingerprint(doc, digest, bindings, Input.ExpectedDigest))
		if err != nil {
			return access.Reply{}, err
		}
		if replayed != nil {
			return *replayed, tx.Commit(r.Context())
		}
	}
	result, err := plan(s, r.Context(), tx, doc, bindings, Input.ExpectedDigest)
	if err != nil {
		return access.Reply{}, err
	}
	if len(result.Conflicts) > 0 || len(result.Blockers) > 0 {
		body := map[string]any{
			"type":      "https://openllmproxy.dev/problems/configuration_not_applicable",
			"title":     http.StatusText(http.StatusConflict),
			"status":    http.StatusConflict,
			"detail":    "The artifact cannot be applied while conflicts or blockers remain.",
			"digest":    result.Digest,
			"actions":   result.Actions,
			"conflicts": result.Conflicts,
			"blockers":  result.Blockers,
		}
		return access.Reply{Status: 409, Body: body}, nil
	}
	if doc.Pricing != nil {
		changed, err := pricingChanged(s, r.Context(), tx, doc)
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
	if err = applyDocument(s, r.Context(), tx, p, doc, bindings); err != nil {
		return access.Reply{}, err
	}
	if err = code.ApplyCode(r.Context(), tx, p, doc); err != nil {
		return access.Reply{}, err
	}
	if err = mcpentries.ApplyMCPServers(s, r.Context(), tx, p, doc, bindings, preparedServers); err != nil {
		return access.Reply{}, err
	}
	if err = retrievalbindings.ApplyRetrievalBindings(s, r.Context(), tx, p, doc); err != nil {
		return access.Reply{}, err
	}
	if err = agentpublications.ApplyAgentPublications(s, r.Context(), tx, p, doc, bindings); err != nil {
		return access.Reply{}, err
	}
	if err = guardrail.ApplyToolsetGuardrailAttachments(r.Context(), tx, p, doc); err != nil {
		return access.Reply{}, err
	}
	// Workload grants may reference resources published by this same apply.
	if err = workloadissuers.ApplyWorkloadIssuers(s, r.Context(), tx, p, doc); err != nil {
		return access.Reply{}, err
	}
	if err = validateAppliedTemplateReferences(r.Context(), tx, doc); err != nil {
		return access.Reply{}, err
	}
	if err := s.Access.ApplySAMLDefinition(r, tx, p, Input.Document.SAML); err != nil {
		return access.Reply{}, err
	}

	if err = access.Audit(r.Context(), tx, r, p.Actor(), "configuration.apply", "configuration", result.Digest, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.OK(result)
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}

func authorizeIdentityPromotion(p access.Principal, doc *promotion.Document) error {
	if doc.SAML != nil || len(doc.WorkloadIssuers) > 0 || len(doc.SCIMGroupMappings) > 0 {
		return p.Authorize(access.Access)
	}
	return nil
}
