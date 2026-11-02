package database

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestUniqueViolationThroughWrapping(t *testing.T) {
	unique := &pgconn.PgError{Code: "23505", ConstraintName: "projects_name"}
	wrapped := fmt.Errorf("insert project: %w", unique)
	if !IsUniqueViolation(wrapped) {
		t.Fatal("wrapped unique violation not detected")
	}
	if !UniqueViolationOn(wrapped, "projects_name") {
		t.Fatal("unique violation on the named constraint not detected")
	}
	if UniqueViolationOn(wrapped, "other_constraint") {
		t.Fatal("unique violation matched the wrong constraint")
	}
	if IsForeignKeyViolation(wrapped) {
		t.Fatal("unique violation reported as a foreign key violation")
	}
	if got := SQLState(wrapped); got != "23505" {
		t.Fatalf("SQLState = %q, want 23505", got)
	}
}

func TestForeignKeyViolationIsDistinct(t *testing.T) {
	foreign := fmt.Errorf("delete provider: %w", &pgconn.PgError{Code: "23503"})
	if !IsForeignKeyViolation(foreign) {
		t.Fatal("foreign key violation not detected")
	}
	if IsUniqueViolation(foreign) {
		t.Fatal("foreign key violation reported as a unique violation")
	}
}

func TestNonPostgresErrorsMatchNothing(t *testing.T) {
	for _, err := range []error{nil, errors.New("23505 in text only")} {
		if IsUniqueViolation(err) || IsForeignKeyViolation(err) || UniqueViolationOn(err, "projects_name") {
			t.Fatalf("%v matched a Postgres violation predicate", err)
		}
		if got := SQLState(err); got != "" {
			t.Fatalf("SQLState(%v) = %q, want empty", err, got)
		}
	}
}
