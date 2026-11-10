package providers

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type grantStart struct {
	SlotID string `json:"slot_id"`
}
type grantContinuation struct {
	Input string `json:"input"`
}

func (s *Server) enrollmentAuthority(r *http.Request) EnrollmentAuthority {
	return EnrollmentAuthority{
		BeginMutation: func(ctx context.Context) (pgx.Tx, access.Principal, error) {
			return s.Access.BeginMutation(r.WithContext(ctx))
		},
		Reauthorize: func(ctx context.Context, tx pgx.Tx) (access.Principal, error) {
			return s.Access.Reauthorize(r.WithContext(ctx), tx)
		},
		Match: func(etag string) error { return access.Match(r, etag) },
		Audit: func(ctx context.Context, tx pgx.Tx, actor access.Actor, action, resource, id, outcome string) error {
			return access.Audit(ctx, tx, r, actor, action, resource, id, outcome)
		},
	}
}

func (s *Server) startGrantEnrollment(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input grantStart
	if r.ContentLength != 0 {
		if err := access.DecodeUnique(r, &input, 1<<10); err != nil {
			return access.Reply{}, err
		}
	}
	if input.SlotID != "" {
		parsed, err := access.ParseUUID(input.SlotID)
		if err != nil {
			return access.Reply{}, access.Invalid("slot_id", "Use a credential slot identifier.")
		}
		input.SlotID = parsed
	}
	enrollment, err := s.enrollments.Start(r.Context(), s.enrollmentAuthority(r), p, id, input.SlotID)
	if err != nil {
		return access.Reply{}, err
	}
	body := map[string]any{"id": enrollment.ID, "provider_id": id, "slot_id": enrollment.SlotID, "expires_at": enrollment.ExpiresAt}
	if device := enrollment.Device; device != nil {
		body["device"] = map[string]any{"verification_url": device.VerificationURL, "user_code": device.UserCode, "interval": device.Interval}
	} else {
		body["authorization_url"] = enrollment.AuthorizationURL
	}
	if enrollment.Input != "" {
		body["input"] = enrollment.Input
	}
	return access.Reply{Status: 201, Body: body}, nil
}

func (s *Server) continueGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	var input grantContinuation
	if err := access.DecodeUnique(r, &input, 2*maxGrantInput); err != nil {
		return access.Reply{}, err
	}
	completed, err := s.enrollments.Continue(r.Context(), s.enrollmentAuthority(r), providerID, enrollmentID, input.Input)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: 201, ETag: completed.ETag, Body: completed}, nil
}

func (s *Server) pollGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	status, err := s.enrollments.Poll(r.Context(), s.enrollmentAuthority(r), providerID, enrollmentID)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(status), nil
}

func (s *Server) cancelGrantEnrollment(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, enrollmentID, err := enrollmentIDs(r)
	if err != nil {
		return access.Reply{}, err
	}
	if err := s.enrollments.Cancel(r.Context(), s.enrollmentAuthority(r), providerID, enrollmentID); err != nil {
		return access.Reply{}, err
	}
	return access.Reply{Status: 204}, nil
}

func enrollmentIDs(r *http.Request) (string, string, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return "", "", err
	}
	enrollmentID, err := access.IDParam(r, "enrollment_id")
	return providerID, enrollmentID, err
}

func (s *Server) auditFailedGrantEnrollment(r *http.Request, actor access.Actor, providerID string) error {
	return s.enrollments.auditFailure(r.Context(), s.enrollmentAuthority(r), actor, providerID)
}
