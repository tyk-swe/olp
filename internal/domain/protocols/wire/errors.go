package wire

import "errors"

// RequestError rejects a request before upstream work. Each ingress codec
// or HTTP boundary chooses its public error representation.
type RequestError struct {
	Code    string
	Message string
	Param   string
}

func (e *RequestError) Error() string { return e.Message }

var ErrEventTooLarge = errors.New("upstream event exceeds the configured size limit")

var ErrAggregateTooLarge = errors.New("aggregated upstream stream exceeds the response size limit")
