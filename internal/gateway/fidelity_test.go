package gateway

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/tyk-swe/olp/internal/routing"
)

type changedFidelityRuntime struct {
	*fakeRuntime
	denied string
	checks atomic.Int64
}

func (r *changedFidelityRuntime) CheckRouteFidelity(_ context.Context, route routing.Route) error {
	r.checks.Add(1)
	if route.Slug == r.denied {
		return errors.New("the installed strict contract is no longer published")
	}
	return nil
}
