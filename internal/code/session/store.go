package session

import (
	"github.com/jackc/pgx/v5/pgxpool"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Admission struct {
	InitialRecovery  bool
	Recovery         *code.Recovery
	Attribution      map[string]string
	Route            code.Route
	APIKeyID         string
	EndUserDigest    string
	Operation        code.Operation
	ProvidersFor     func(model string) []string
	Bound            *code.TokenBound
	PreviousResponse string
}

func StaticProviders(providers ...string) func(string) []string {
	return func(string) []string { return providers }
}

func (in Admission) providers() []string {
	if in.ProvidersFor == nil {
		return nil
	}
	return in.ProvidersFor(in.Operation.Model)
}

func (in Admission) withOperation(operation code.Operation) Admission {
	updated := in
	updated.Operation = operation
	return updated
}

type Permit struct {
	InitialRecovery bool
	Authority       keyauthority.Authority
	Binding         code.Binding
	Pin             code.Pin
	Account         code.Account
	Attempt         code.Attempt
}
