package database

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	uniqueViolationCode     = "23505"
	foreignKeyViolationCode = "23503"
)

func pgError(err error) *pgconn.PgError {
	problem, _ := errors.AsType[*pgconn.PgError](err)
	return problem
}

func IsUniqueViolation(err error) bool {
	problem := pgError(err)
	return problem != nil && problem.Code == uniqueViolationCode
}

func UniqueViolationOn(err error, constraint string) bool {
	problem := pgError(err)
	return problem != nil && problem.Code == uniqueViolationCode && problem.ConstraintName == constraint
}

func IsForeignKeyViolation(err error) bool {
	problem := pgError(err)
	return problem != nil && problem.Code == foreignKeyViolationCode
}

func SQLState(err error) string {
	if problem := pgError(err); problem != nil {
		return problem.Code
	}
	return ""
}
