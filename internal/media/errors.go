package media

import (
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/staging"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func Fail(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func invalidMedia(message string) *Error {
	return Fail(http.StatusBadRequest, "invalid_request", message)
}

func spoolError(err error) *Error {
	if _, ok := errors.AsType[*staging.TooLargeError](err); ok {
		return Fail(http.StatusRequestEntityTooLarge, "media_too_large", "The media body exceeds the configured limit.")
	}
	if errors.Is(err, staging.ErrInvalidFilename) || errors.Is(err, staging.ErrInvalidHandle) || errors.Is(err, staging.ErrZeroLimit) {
		return invalidMedia(err.Error())
	}
	return Fail(http.StatusServiceUnavailable, "media_spool_unavailable", "The media staging area is unavailable.")
}
