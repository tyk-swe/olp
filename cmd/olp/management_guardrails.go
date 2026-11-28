package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/guardrails/credential"
	"github.com/tyk-swe/olp/internal/guardrails/definition"
	"github.com/tyk-swe/olp/internal/guardrails/policy"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routes/guardrailpreview"
	"github.com/tyk-swe/olp/internal/routing"
)

func (m Management) registerGuardrails(mux *http.ServeMux) {
	publish := func(ctx context.Context, tx pgx.Tx, actor string) error {
		_, err := release.Publish(ctx, tx, actor)
		return err
	}
	credential.NewAPI(m.Access).Register(mux)
	definition.NewAPI(m.Access, publish).Register(mux)
	policy.NewAPI(m.Access, publish, func(r *http.Request, p authz.Principal, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
		result, err := m.Gateway.EvaluateGuardrails(r, guardrailsMember(p), pinned, in)
		return result, guardrailsHTTPError(err)
	}, func(r *http.Request, p authz.Principal, batch []guardrails.PinnedApplication) ([]guardrails.Result, error) {
		results, err := m.Gateway.EvaluateGuardrailsBatch(r, guardrailsMember(p), batch)
		return results, guardrailsHTTPError(err)
	}).Register(mux)
	attachment.NewAPI(m.Access, publish).Register(mux)
	guardrailpreview.Register(m.Access, mux, m.Runtime.Release)
}

func guardrailsMember(p authz.Principal) kernel.Member {
	return kernel.Member{
		UserID: p.UserID(),
		Admit: func(route *routing.Route) *fault.Error {
			if err := p.Authorize(authz.Playground); err != nil {
				return fault.PermissionError("guardrail_forbidden", "Playground access is required.")
			}
			if err := p.Project(route.ProjectID, authz.View); err != nil {
				return fault.ModelNotFound(route.Slug)
			}
			return nil
		},
	}
}

func guardrailsHTTPError(err error) error {
	failure, ok := errors.AsType[*fault.Error](err)
	if !ok {
		return err
	}
	if failure.Code == "guardrails_unavailable" || failure.Code == fault.Overloaded.Code {
		return httpkit.Fail(failure.Status, failure.Code, failure.Message)
	}
	return httpkit.Fail(failure.Status, failure.Code, "The guardrail could not be evaluated.")
}
