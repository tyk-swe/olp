package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/oif"
)

const endUserHeader = "X-OLP-End-User"

type endUserIdentityKey struct{}

// A second authorization check on the same request must not reread a body
// already consumed by its protocol parser. Only the digest is retained here.
type endUserIdentity struct{ key, project, digest string }

// authenticateRequest binds an authenticated caller to its declared end-user
// source. The cached authority is copied; only the digest escapes the request.
func (s *Server) identifyEndUser(r *http.Request, authority access.Authority) (access.Authority, *Error) {
	if authority.WorkloadIssuerID != nil && authority.EndUserDigest != "" {
		return authority, nil
	}
	if authority.Policy.EndUserSource == nil {
		if authority.ProjectEndUserPolicy != nil || authority.Policy.EndUserPolicy != nil {
			return access.Authority{}, invalidRequest("end_user_source_required", "This key must configure an end-user identifier source before using an end-user policy.", nil)
		}
		return authority, nil
	}
	identifier := ""
	switch *authority.Policy.EndUserSource {
	case "header":
		values := r.Header.Values(endUserHeader)
		if len(values) == 1 {
			identifier = values[0]
		}
	case "native":
		project := ""
		if authority.ProjectID != nil {
			project = *authority.ProjectID
		}
		if identity, ok := r.Context().Value(endUserIdentityKey{}).(endUserIdentity); ok && identity.key == authority.ID && identity.project == project {
			authority.EndUserDigest = identity.digest
			return authority, nil
		}
		// Native identification is defined only for JSON OpenAI and Anthropic
		// bodies. Other surfaces must use the header source.
		if surface := requestSurface(r); surface == "openai" || surface == "anthropic" {
			// Inspect decoded JSON while retaining the bounded original wire body
			// for strict forwarding, including its content encoding.
			var wire bytes.Buffer
			original := r.Body
			r.Body = io.NopCloser(io.TeeReader(original, &wire))
			body, err := s.readBody(r)
			_ = original.Close()
			if err != nil {
				return access.Authority{}, err
			}
			if measured, ok := original.(*measuredBody); ok {
				measured.decoded = int64(len(body))
			}
			r.Body = io.NopCloser(bytes.NewReader(wire.Bytes()))
			r.ContentLength = int64(wire.Len())
			identifier = nativeEndUser(body, surface)
		}
	}
	if !access.ValidEndUserIdentifier(identifier) {
		return access.Authority{}, invalidRequest("invalid_end_user", "Provide an end-user machine token of 1–128 characters in the key's configured source.", nil)
	}
	digester, ok := s.Runtime.(interface{ EndUserDigest(*string, string) string })
	if !ok {
		return access.Authority{}, serverError(http.StatusServiceUnavailable, "authority_unavailable", "End-user identity is unavailable.")
	}
	authority.EndUserDigest = digester.EndUserDigest(authority.ProjectID, identifier)
	if *authority.Policy.EndUserSource == "native" {
		identity := endUserIdentity{key: authority.ID, digest: authority.EndUserDigest}
		if authority.ProjectID != nil {
			identity.project = *authority.ProjectID
		}
		*r = *r.WithContext(context.WithValue(r.Context(), endUserIdentityKey{}, identity))
	}
	return authority, nil
}

func nativeEndUser(body []byte, surface string) string {
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return ""
	}
	path := "/safety_identifier"
	if surface == "anthropic" {
		path = "/metadata/user_id"
	}
	value, present := document.Lookup(path)
	if !present && surface == "openai" {
		value, _ = document.Lookup("/user")
	}
	identifier, _ := value.Text()
	return identifier
}
