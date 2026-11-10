package media

import (
	"net/http"
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
