package ingress

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/oif"
)

type endUserIdentityKey struct{}

// A second authorization check on the same request must not reread a body
// already consumed by its protocol parser. Only the digest is retained here.
type endUserIdentity struct{ key, project, digest string }

// AuthenticateRequest binds an authenticated caller to its declared end-user
// source. The cached authority is copied; only the digest escapes the request.
func (g *Gate) IdentifyEndUser(r *http.Request, authority keyauthority.Authority) (keyauthority.Authority, *fault.Error) {
	if authority.TaskCapability != nil || authority.WorkloadIssuerID != nil && authority.EndUserDigest != "" {
		return authority, nil
	}
	if authority.Policy.EndUserSource == nil {
		if authority.ProjectEndUserPolicy != nil || authority.Policy.EndUserPolicy != nil {
			return keyauthority.Authority{}, fault.InvalidRequest("end_user_source_required", "This key must configure an end-user identifier source before using an end-user policy.", nil)
		}
		return authority, nil
	}
	identifier := ""
	switch *authority.Policy.EndUserSource {
	case "header":
		digest, failure := consumerauth.HeaderEndUserDigest(g.auth, r, authority)
		if failure != nil {
			if failure.Status == http.StatusBadRequest {
				return keyauthority.Authority{}, fault.InvalidRequest(failure.Code, failure.Message, nil)
			}
			return keyauthority.Authority{}, fault.ServerError(failure.Status, failure.Code, failure.Message)
		}
		authority.EndUserDigest = digest
		return authority, nil
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
		if surface := fault.RequestSurface(r); surface == "openai" || surface == "anthropic" {
			// Inspect decoded JSON while retaining the bounded original wire body
			// for strict forwarding, including its content encoding.
			var encodedWire bytes.Buffer
			original := r.Body
			r.Body = io.NopCloser(io.TeeReader(original, &encodedWire))
			body, err := g.ReadBody(r)
			_ = original.Close()
			if err != nil {
				return keyauthority.Authority{}, err
			}
			if measured, ok := original.(*MeasuredBody); ok {
				measured.Decoded = int64(len(body))
			}
			r.Body = io.NopCloser(bytes.NewReader(encodedWire.Bytes()))
			r.ContentLength = int64(encodedWire.Len())
			identifier = nativeEndUser(body, surface)
		}
	}
	if !access.ValidEndUserIdentifier(identifier) {
		return keyauthority.Authority{}, fault.InvalidRequest("invalid_end_user", "Provide an end-user machine token of 1–128 characters in the key's configured source.", nil)
	}
	digester, ok := g.auth.(interface{ EndUserDigest(*string, string) string })
	if !ok {
		return keyauthority.Authority{}, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "End-user identity is unavailable.")
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
	document, err := ParseObjectRequest(body, oif.Limits{})
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
