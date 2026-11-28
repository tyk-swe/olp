package httpkit

import (
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/database"
)

// TODO: remove this file once #1165 and #1168 merge. They edit the last callers of these names, so rewriting those files now would conflict with them.

func Fail(status int, code, detail string) error { return problem.Fail(status, code, detail) }

func Invalid(field, detail string) error { return problem.Invalid(field, detail) }

func NotFoundOr(err error) error { return database.NotFoundOr(err) }

func ValidText(field, value string, limit int) error {
	return textfield.Validate(field, value, limit)
}

func ParseUUID(value string) (string, error) { return ids.Parse(value) }
