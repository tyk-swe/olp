package httpkit

import (
	"context"

	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type Problem = problem.Problem

type Pagination = database.Pagination

var NotFound = problem.NotFound

func Fail(status int, code, detail string) error { return problem.Fail(status, code, detail) }

func Invalid(field, detail string) error { return problem.Invalid(field, detail) }

func Forbidden() error { return problem.Forbidden() }

func ContractProblem(err error) *Problem { return problem.FromContract(err) }

func NotFoundOr(err error) error { return database.NotFoundOr(err) }

func QueryJSON(ctx context.Context, db database.Queryer, sql string, args ...any) ([]map[string]any, error) {
	return database.QueryJSON(ctx, db, sql, args...)
}

func ValidText(field, value string, max int) error { return textfield.Validate(field, value, max) }

func ParseUUID(value string) (string, error) { return ids.Parse(value) }

func ParseUUIDs(values []string) ([]string, error) { return ids.ParseAll(values) }

func ParseOptionalUUID(value *string) (*string, error) { return ids.ParseOptional(value) }

func ParseProjectID(value *string) (*string, error) { return ids.ParseProject(value) }
