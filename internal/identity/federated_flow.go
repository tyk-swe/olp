package identity

import (
	"context"
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

const (
	maxFederatedReturnLen = 2048
	expiredCleanupLimit   = 100
)

type federatedBeginInput struct {
	ReturnTo   string `json:"return_to"`
	Purpose    string `json:"purpose"`
	ResourceID string `json:"resource_id"`
}

func parseFederatedBeginInput(r *http.Request, kind string, allowQuery bool) (federatedBeginInput, error) {
	var input federatedBeginInput
	if allowQuery && r.Method == http.MethodGet {
		input.ReturnTo = r.URL.Query().Get("return_to")
	} else if r.ContentLength != 0 {
		if err := httpkit.Decode(r, &input); err != nil {
			return federatedBeginInput{}, err
		}
	}
	if !safeReturn(input.ReturnTo) || len(input.ReturnTo) > maxFederatedReturnLen {
		return federatedBeginInput{}, httpkit.Invalid("return_to", "Use a local console path.")
	}
	if input.ReturnTo == "" {
		input.ReturnTo = "/"
		if kind == "link" {
			input.ReturnTo = "/settings/profile"
		}
	}
	if kind == "reauthenticate" {
		if err := validatePurpose(input.Purpose, input.ResourceID); err != nil {
			return federatedBeginInput{}, err
		}
	} else if input.Purpose != "" || input.ResourceID != "" {
		return federatedBeginInput{}, httpkit.Invalid("purpose", "This flow has no recent-authentication purpose.")
	}
	return input, nil
}

func reauthenticatedLocation(purpose, resourceID string) string {
	location := reauthenticatedAt(purpose) + "?reauthenticated=" + url.QueryEscape(purpose)
	if resourceID != "" {
		location += "&resource_id=" + url.QueryEscape(resourceID)
	}
	return location
}

func pruneExpiredSecrets(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id IN (SELECT id FROM olp.secrets WHERE expires_at<=now() LIMIT $1)", expiredCleanupLimit)
	return err
}

func (s *Server) authenticateFlowSession(r *http.Request, tx pgx.Tx, expectedUserID, expectedSessionID, mismatchCode, mismatchMsg string) (authz.Principal, error) {
	p, err := s.access.Authenticate(r, tx)
	if err != nil {
		return authz.Principal{}, err
	}
	if err = p.Authorize(authz.Self); err != nil {
		return authz.Principal{}, err
	}
	if p.ID != expectedUserID || p.SessionID != expectedSessionID {
		return authz.Principal{}, httpkit.Fail(403, mismatchCode, mismatchMsg)
	}
	return p, nil
}

func (s *Server) runFlowTx(r *http.Request, fn func(pgx.Tx) (httpkit.Reply, bool, error)) (httpkit.Reply, error) {
	tx, err := s.access.Begin(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	reply, commit, err := fn(tx)
	if commit {
		if commitErr := tx.Commit(r.Context()); commitErr != nil {
			return httpkit.Reply{}, commitErr
		}
	}
	return reply, err
}
