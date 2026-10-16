package usage

import (
	"context"
	"errors"
)

type recordingStream struct {
	payloads []string
	failOnce bool
}

func (s *recordingStream) Do(_ context.Context, args ...string) (any, error) {
	if s.failOnce {
		s.failOnce = false
		return nil, errors.New("temporary outage")
	}
	s.payloads = append(s.payloads, args[4])
	return "1-0", nil
}
