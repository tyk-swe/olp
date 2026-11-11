package reports

import (
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type FailureKind string

const (
	FailureInvalidRange       FailureKind = "invalid_range"
	FailureInvalidOperation   FailureKind = "invalid_operation"
	FailureInvalidFilter      FailureKind = "invalid_filter"
	FailureInvalidDimension   FailureKind = "invalid_dimension"
	FailureInvalidGranularity FailureKind = "invalid_granularity"
	FailureExportTooLarge     FailureKind = "export_too_large"
)

// Failure is a report request the caller has to change.
type Failure struct {
	Kind   FailureKind
	Detail string
}

func (f *Failure) Error() string { return string(f.Kind) }

func fail(kind FailureKind, detail string) error { return &Failure{Kind: kind, Detail: detail} }

func problem(err error) error {
	failure, ok := errors.AsType[*Failure](err)
	if !ok {
		return err
	}
	status := http.StatusBadRequest
	if failure.Kind == FailureExportTooLarge {
		status = http.StatusUnprocessableEntity
	}
	return access.Fail(status, string(failure.Kind), failure.Detail)
}
