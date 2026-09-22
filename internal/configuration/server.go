package configuration

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

type Server struct {
	Access                 *access.Server
	Egress                 *egress.Policy
	VendorKind             func(vendor string) (string, bool)
	StoreNetworkCredential func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
	StoreCredential        func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/configuration/export", s.Access.Handle(s.export))
	mux.HandleFunc("POST /api/v3/configuration/plan", s.Access.HandleWith(4<<20, s.planEndpoint))
	mux.HandleFunc("POST /api/v3/configuration/apply", s.Access.HandleTimeout(4<<20, 60*time.Second, s.applyEndpoint))
}

func (s *Server) principal(r *http.Request, q access.Queryer) (access.Principal, error) {
	p, err := s.Access.Principal(r, q, "configure")
	if err != nil {
		return p, err
	}
	if !p.AllProjects {
		return p, access.Forbidden()
	}
	return p, nil
}

func (s *Server) export(r *http.Request) (access.Reply, error) {
	if _, err := s.principal(r, s.Access.Pool); err != nil {
		return access.Reply{}, err
	}
	doc, err := s.exportDocument(r.Context(), s.Access.Pool)
	if err != nil {
		return access.Reply{}, err
	}
	digest, err := Digest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	doc.ExportedAt = time.Now().UTC().Format(time.RFC3339)
	return access.OK(map[string]any{"digest": digest, "document": doc}), nil
}

type promotionInput struct {
	Document       *Document         `json:"document"`
	ExpectedDigest *string           `json:"expected_digest"`
	SecretBindings map[string]string `json:"secret_bindings"`
}

func (s *Server) planEndpoint(r *http.Request) (access.Reply, error) {
	var input promotionInput
	if err := access.DecodeUnique(r, &input,4<<20); err != nil {
		return access.Reply{}, err
	}
	if input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	if _, err := s.principal(r, s.Access.Pool); err != nil {
		return access.Reply{}, err
	}
	result, err := s.plan(r.Context(), s.Access.Pool, input.Document, input.SecretBindings, input.ExpectedDigest)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(result), nil
}

func (s *Server) applyEndpoint(r *http.Request) (access.Reply, error) {
	var input promotionInput
	if err := access.DecodeUnique(r, &input,4<<20); err != nil {
		return access.Reply{}, err
	}
	if input.Document == nil {
		return access.Reply{}, access.Invalid("document", "Send the configuration artifact.")
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	doc := input.Document
	doc.canonicalize()
	digest, err := Digest(doc)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, bindingFingerprint(doc, digest, input.SecretBindings, input.ExpectedDigest))
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	result, err := s.plan(r.Context(), tx, doc, input.SecretBindings, input.ExpectedDigest)
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
		if changed {
			if _, err := s.Access.Principal(r, tx, "settings"); err != nil {
				return access.Reply{}, err
			}
		}
	}
	if err = s.applyDocument(r.Context(), tx, p, doc, input.SecretBindings); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "configuration.apply", "configuration", digest, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.OK(result)
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
