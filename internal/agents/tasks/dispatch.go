package tasks

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/limits"
)

type State string

const (
	Active    State = "active"
	Unknown   State = "outcome_unknown"
	Completed State = "completed"
	Canceled  State = "canceled"
	Failed    State = "failed"
)

var ErrRateLimit = errors.New("agent send rate exceeded")
var ErrTaskLimit = errors.New("agent task limit reached")
var ErrNotFound = errors.New("agent task not found")

type SendIntent struct {
	Authority access.Authority
	Revision  agentcontract.Revision
	ReleaseID string
	TaskID    string
	Binding   string
}
type Outcome struct {
	UpstreamID string
	ContextID  string
	State      State
}
type Dispatch interface {
	Capability() string
	MarkDispatched(context.Context) error
	Settle(context.Context, Outcome) (string, error)
	Abort(context.Context)
	Unknown(context.Context)
	Finish(context.Context)
}
type dispatch struct {
	store       *Store
	reservation reservation
	intent      SendIntent
	capability  string
	leases      []*limits.Lease
}

func (s *Store) Prepare(ctx context.Context, in SendIntent) (Dispatch, error) {
	res, err := s.reserve(ctx, in.Authority, in.Revision, in.ReleaseID)
	if err != nil {
		return nil, err
	}
	capability, admissionErr := s.admitTask(ctx, res, in.Authority, in.TaskID, in.Revision)
	if admissionErr != nil {
		s.release(ctx, res)
		return nil, ErrTaskLimit
	}
	leases, rateErr := s.admitRate(ctx, in.Authority)
	if rateErr != nil {
		s.release(ctx, res)
		return nil, ErrRateLimit
	}
	return &dispatch{store: s, reservation: res, intent: in, capability: capability, leases: leases}, nil
}
func (d *dispatch) Capability() string { return d.capability }
func (d *dispatch) MarkDispatched(ctx context.Context) error {
	return d.store.debits.MarkDispatched(ctx, d.reservation.DispatchID, d.reservation.ClaimID)
}
func (d *dispatch) Settle(ctx context.Context, outcome Outcome) (string, error) {
	return d.store.settle(ctx, d.reservation, d.intent.Authority, d.intent.Revision, outcome.UpstreamID, outcome.ContextID, outcome.State, d.intent.Binding)
}
func (d *dispatch) Abort(ctx context.Context) {
	d.store.release(ctx, d.reservation)
	finishRate(ctx, d.leases, false)
}
func (d *dispatch) Unknown(ctx context.Context) { d.store.unknown(ctx, d.reservation) }
func (d *dispatch) Finish(ctx context.Context)  { finishRate(ctx, d.leases, true) }
