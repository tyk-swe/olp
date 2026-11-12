package probe

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
)

type Reason string

const (
	CapabilityUnavailable        Reason = "capability_unavailable"
	ClientCancelled              Reason = "client_cancelled"
	ControlUnavailable           Reason = "control_unavailable"
	CredentialInvalid            Reason = "credential_invalid"
	GatewayTimeout               Reason = "gateway_timeout"
	InvalidEndpoint              Reason = "invalid_endpoint"
	InvalidRequest               Reason = "invalid_request"
	ModelInvalid                 Reason = "model_invalid"
	ModelRequired                Reason = "model_required"
	ModelUnavailable             Reason = "model_unavailable"
	NetworkCredentialInvalid     Reason = "network_credential_invalid"
	NoCapabilities               Reason = "no_capabilities"
	NoEnabledModels              Reason = "no_enabled_models"
	ProviderProtocolError        Reason = "provider_protocol_error"
	UnsafeDestination            Reason = "unsafe_destination"
	UpstreamAuthenticationFailed Reason = "upstream_authentication_failed"
	UpstreamNotFound             Reason = "upstream_not_found"
	UpstreamPermissionDenied     Reason = "upstream_permission_denied"
	UpstreamRateLimit            Reason = "upstream_rate_limit"
	UpstreamRedirect             Reason = "upstream_redirect"
	UpstreamRejected             Reason = "upstream_rejected"
	UpstreamResponseTooLarge     Reason = "upstream_response_too_large"
	UpstreamUnavailable          Reason = "upstream_unavailable"
)

// A Failure detail never carries upstream text, which may echo credentials.
type Failure struct {
	Reason Reason
	Detail string
}

func (f *Failure) Error() string { return f.Detail }

func Classify(err error) *Failure {
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure
	}
	switch {
	case errors.Is(err, egress.ErrUnsafeDestination):
		return &Failure{Reason: UnsafeDestination, Detail: "The endpoint resolves to a blocked address."}
	case errors.Is(err, egress.ErrRedirect):
		return &Failure{Reason: UpstreamRedirect, Detail: "The upstream answered with a redirect, which the gateway refuses."}
	case errors.Is(err, context.DeadlineExceeded):
		return &Failure{Reason: GatewayTimeout, Detail: "The upstream did not answer within the probe deadline."}
	case errors.Is(err, context.Canceled):
		return &Failure{Reason: ClientCancelled, Detail: "The probe was cancelled."}
	}
	return &Failure{Reason: UpstreamUnavailable, Detail: "The upstream could not be reached."}
}

func statusError(t Target, status int, body []byte) *Failure {
	transport := t.Connection.Transport()
	status, body = transport.Rejection(status, body)
	evidence := upstream.Evidence{Status: status, Error: openai.ParseErrorBody(body)}
	class := upstream.Classifier{Declared: transport.Classification()}.Classify(evidence).Class
	switch {
	case class == upstream.Credential && status == http.StatusForbidden:
		return &Failure{Reason: UpstreamPermissionDenied, Detail: fmt.Sprintf("The upstream denied access (HTTP %d).", status)}
	case class == upstream.Credential:
		return &Failure{Reason: UpstreamAuthenticationFailed, Detail: fmt.Sprintf("The upstream rejected the credential (HTTP %d).", status)}
	case class == upstream.RateLimit:
		return &Failure{Reason: UpstreamRateLimit, Detail: fmt.Sprintf("The upstream is rate limiting (HTTP %d).", status)}
	case class == upstream.ServerError:
		return &Failure{Reason: UpstreamUnavailable, Detail: fmt.Sprintf("The upstream failed (HTTP %d).", status)}
	case status == http.StatusNotFound:
		return &Failure{Reason: UpstreamNotFound, Detail: "The upstream has no such resource (HTTP 404)."}
	}
	return &Failure{Reason: UpstreamRejected, Detail: fmt.Sprintf("The upstream answered HTTP %d.", status)}
}
