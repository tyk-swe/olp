package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type Code string

const (
	CodeEndpoint   Code = "endpoint"
	CodeTimeout    Code = "timeout"
	CodeNetwork    Code = "network"
	CodeCredential Code = "credential"
	CodeStatus     Code = "status"
	CodeResponse   Code = "response"
)

type Failure struct {
	Code Code
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }

func (f *Failure) Unwrap() error { return f.Err }

func fail(code Code, err error) error { return &Failure{Code: code, Err: err} }

func CodeOf(err error) Code {
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure.Code
	}
	return CodeNetwork
}

var errMissing = errors.New("the vendor has no such object")

func statusFailure(status int) error {
	switch status {
	case http.StatusNotFound:
		return fail(CodeStatus, errMissing)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fail(CodeCredential, fmt.Errorf("vendor answered %d", status))
	default:
		return fail(CodeStatus, fmt.Errorf("vendor answered %d", status))
	}
}

func transportFailure(ctx context.Context, err error) error {
	if timeout, ok := errors.AsType[net.Error](err); errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || ok && timeout.Timeout() {
		return fail(CodeTimeout, err)
	}
	if errors.Is(err, egress.ErrRedirect) {
		return fail(CodeEndpoint, err)
	}
	return fail(CodeNetwork, err)
}

func storageFailure(ctx context.Context, err error) error {
	if response, ok := errors.AsType[interface {
		error
		HTTPStatusCode() int
	}](err); ok {
		return statusFailure(response.HTTPStatusCode())
	}
	if response, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return statusFailure(response.StatusCode)
	}
	if errors.Is(err, connectors.ErrAuthentication) || errors.Is(err, connectors.ErrCredentialRejected) {
		return fail(CodeCredential, err)
	}
	return transportFailure(ctx, err)
}
