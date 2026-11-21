package operatorcli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/tyk-swe/olp/management"
)

type errorReport struct {
	Kind   management.ErrorKind `json:"kind"`
	Status int                  `json:"status,omitempty"`
	Code   string               `json:"code,omitempty"`
}

type commandError struct {
	report errorReport
	cause  error
}

func (err *commandError) Error() string { return err.cause.Error() }
func (err *commandError) Unwrap() error { return err.cause }

func commandFailure(kind management.ErrorKind, cause error) *commandError {
	return &commandError{report: errorReport{Kind: kind}, cause: cause}
}

func callFailure(failure *management.CallError) *commandError {
	return &commandError{report: errorReport{Kind: failure.Kind, Status: failure.Status, Code: failure.Code}, cause: failure.Unwrap()}
}

func WriteJSONError(w io.Writer, err error) bool {
	failure, ok := errors.AsType[*commandError](err)
	if !ok {
		return false
	}
	json.NewEncoder(w).Encode(failure.report)
	return true
}
