package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

// codeRefusal is the refusal a client sees for err; failures that are not
// refusals do not leak their detail.
func codeRefusal(err error) *codemode.Refusal {
	if refusal, ok := errors.AsType[*codemode.Refusal](err); ok {
		return refusal
	}
	return &codemode.Refusal{Status: 503, Code: "code_service_unavailable"}
}

// codeWriteError writes a refusal in the envelope of the request's surface:
// Anthropic's for a route's Messages paths, OpenAI's otherwise.
func codeWriteError(w http.ResponseWriter, r *http.Request, err error) {
	refusal := codeRefusal(err)
	surface, kind := requestSurface(r), "code_mode_error"
	if surface == "anthropic" {
		switch refusal.Status {
		case 400, 409, 413, 415, 422:
			kind = "invalid_request_error"
		case 401:
			kind = "authentication_error"
		case 403:
			kind = "permission_error"
		}
	}
	writeSurfaceError(w, &Error{Status: refusal.Status, Code: refusal.Code, Type: kind, Message: codeRefusalMessage(refusal.Code)}, surface)
}

func (s *Server) codeRefuse(w http.ResponseWriter, r *http.Request, route codemode.Route, keyID, digest string, err error) {
	refusal := codeRefusal(err)
	s.recordCodeRefusal(r.Context(), route, keyID, digest, refusal.Code)
	codeWriteError(w, r, refusal)
}

func (s *Server) recordCodeRefusal(ctx context.Context, route codemode.Route, keyID, digest, code string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = s.CodeLedger.RecordRefusal(ctx, route, keyID, digest, code)
}

func codeRefusalMessage(code string) string {
	switch code {
	case "code_recovery_unsafe", "code_account_unavailable":
		return "Continue on the original account when available, or start a new conversation with full context and no response references, reasoning signatures or socket state"
	case "code_parent_unresolved":
		return "Establish the parent conversation first, or start a new conversation with full context and no response reference"
	case "code_response_account_conflict":
		return "Continue on the account that produced the response, or start a new conversation with full context and no response reference"
	case "code_translation_unsupported_semantics":
		return "This route supports text, tools and explicit reasoning effort. Use a native Messages account for required semantics this route cannot represent."
	}
	return code
}
