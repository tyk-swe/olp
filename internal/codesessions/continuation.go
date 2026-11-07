package codesessions

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/secrets"
)

type SignedCall struct {
	ID         string  `json:"id"`
	Group      string  `json:"group"`
	Index      int     `json:"index"`
	Count      int     `json:"count"`
	Digest     string  `json:"digest"`
	ProviderID *string `json:"provider_id,omitempty"`
	Signature  string  `json:"signature,omitempty"`
}

type signedContinuationRecord struct {
	RootID    string     `json:"root_id"`
	AccountID string     `json:"account_id"`
	Principal string     `json:"principal"`
	Model     string     `json:"model"`
	Call      SignedCall `json:"call"`
}

func NewEncrypted(pool *pgxpool.Pool, installation string, keys *secrets.KeyRing) *Store {
	return &Store{pool: pool, installation: installation, keys: keys}
}

func (s *Store) SaveCodeContinuation(ctx context.Context, permit Permit, model string, calls []SignedCall) error {
	if s.keys == nil || len(calls) == 0 {
		return codemode.Refuse(503, "code_continuation_unavailable")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := liveCodeTree(ctx, tx, permit.Binding); err != nil {
		return err
	}
	for _, call := range calls {
		data, err := json.Marshal(signedContinuationRecord{RootID: permit.Binding.RootID, AccountID: permit.Account.ID, Principal: permit.Account.Principal, Model: model, Call: call})
		if err != nil || len(data) > codewire.MaxBody {
			return codemode.Refuse(502, "code_upstream_unsupported_state")
		}
		if err := s.keys.Store(ctx, tx, s.installation, call.ID, secrets.CodeContinuation, data, nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO olp.code_continuations(id,root_id,account_id,principal,model) VALUES($1,$2,$3,$4,$5)`, call.ID, permit.Binding.RootID, permit.Account.ID, permit.Account.Principal, model); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) LoadCodeContinuation(ctx context.Context, permit Permit, model, id string) (SignedCall, error) {
	refused := codemode.Refuse(422, "code_continuation_unavailable")
	if s.keys == nil {
		return SignedCall{}, refused
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SignedCall{}, refused
	}
	defer tx.Rollback(ctx)
	if err := liveCodeTree(ctx, tx, permit.Binding); err != nil {
		return SignedCall{}, refused
	}
	var found string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM olp.code_continuations WHERE id=$1 AND root_id=$2 AND account_id=$3 AND principal=$4 AND model=$5`, id, permit.Binding.RootID, permit.Account.ID, permit.Account.Principal, model).Scan(&found); err != nil {
		return SignedCall{}, refused
	}
	data, err := s.keys.Read(ctx, tx, s.installation, found, secrets.CodeContinuation)
	if err != nil {
		return SignedCall{}, refused
	}
	var record signedContinuationRecord
	if err := json.Unmarshal(data, &record); err != nil || record.Call.ID != id || record.RootID != permit.Binding.RootID || record.AccountID != permit.Account.ID || record.Principal != permit.Account.Principal || record.Model != model {
		return SignedCall{}, refused
	}
	return record.Call, tx.Commit(ctx)
}
