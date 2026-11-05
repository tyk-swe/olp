package mcp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
)

type ToolAuthority interface {
	AuthorizeMCPTool(access.Authority, string, string, string, string, string) error
	AdmitTaskCapabilityAttempt(context.Context, access.Authority, string, string, string, *string, string) error
	VerifyEntraUser(context.Context, string, access.Authority, string, string) (workload.UserAssertion, error)
}

type GrantResolver interface {
	Resolve(context.Context, access.Authority, routing.MCPToolsetTool) ([]byte, error)
	ResolveEntra(context.Context, access.Authority, routing.MCPToolsetTool, workload.UserAssertion) ([]byte, error)
	Delegate(context.Context, access.Authority, routing.MCPToolsetTool) ([]byte, error)
	CheckGrant(context.Context, access.Authority, routing.MCPToolsetTool) error
}

type ExecutorOptions struct {
	Authority   ToolAuthority
	Grants      GrantResolver
	Connect     upstream.Dial
	Ledger      accounting.Ledger
	Limiter     RateLimiter
	Auth        *secrets.AuthKey
	Log         *slog.Logger
	Credentials func(context.Context, string) ([]byte, error)
	Guardrails  func(context.Context, GuardrailEvaluation) (guardrails.Result, error)
	Now         func() time.Time
}

type Executor struct {
	authority   ToolAuthority
	grants      GrantResolver
	connect     upstream.Dial
	ledger      accounting.Ledger
	limiter     RateLimiter
	auth        *secrets.AuthKey
	log         *slog.Logger
	credentials func(context.Context, string) ([]byte, error)
	guardrails  func(context.Context, GuardrailEvaluation) (guardrails.Result, error)
	now         func() time.Time
	results     *resultCache
}

func NewExecutor(options ExecutorOptions) *Executor {
	logger := func() *slog.Logger {
		if options.Log != nil {
			return options.Log
		}
		return slog.Default()
	}()
	now := func() func() time.Time {
		if options.Now != nil {
			return options.Now
		}
		return time.Now
	}()
	return &Executor{
		authority: options.Authority, grants: options.Grants, connect: options.Connect,
		ledger: options.Ledger, limiter: options.Limiter, auth: options.Auth, log: logger,
		credentials: options.Credentials, guardrails: options.Guardrails, now: now,
		results: &resultCache{entries: make(map[string]retainedResult)},
	}
}

func (s *Executor) available() bool {
	return s != nil && s.authority != nil && s.ledger != nil && s.auth != nil && s.connect != nil
}
func (s *Executor) finishLeases(ctx context.Context, leases []*limits.Lease, dispatched bool) {
	for _, lease := range leases {
		finish := lease.Refund
		action := "refund"
		if dispatched {
			finish = lease.Release
			action = "release"
		}
		if err := finish(context.WithoutCancel(ctx)); err != nil {
			s.log.Warn("tool rate lease "+action+" failed, retrying once", "error", err)
			if retryErr := finish(context.WithoutCancel(ctx)); retryErr != nil {
				s.log.Warn("tool rate lease "+action+" failed after retry", "error", retryErr)
			}
		}
	}
}

func (s *Executor) release(reservation *accounting.Reservation) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.ledger.ReleaseUndispatched(ctx, reservation)
}

func (s *Executor) toolInputDigest(arguments json.RawMessage) string {
	return hex.EncodeToString(s.auth.Digest(secrets.ToolInputDigest, string(arguments)))
}
func executeCallIDFailure(callID string) *Failure {
	if callID == "" {
		return failureCallIDRequired
	}
	if !validCallID(callID) {
		return failureCallIDInvalid
	}
	return nil
}
