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
	if code == "code_translation_unsupported_semantics" {
		return "This route translates text only. Use a native Messages account for tools, thinking, beta features, stop sequences or other required extensions."
	}
	return code
}
