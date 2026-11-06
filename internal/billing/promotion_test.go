package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type applyImportRow struct {
	state *ImportState
}

func (row applyImportRow) Scan(dest ...any) error {
	if row.state == nil {
		return pgx.ErrNoRows
	}
	*dest[0].(*string) = row.state.Source
	*dest[1].(*bool) = row.state.Enabled
	*dest[2].(*json.RawMessage) = row.state.Scope
	*dest[3].(**string) = row.state.CredentialID
	return nil
}

type applyImportTx struct {
	pgx.Tx
	row       pgx.Row
	execErrs  []error
	execCalls int
}

func (tx *applyImportTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return tx.row
}

func (tx *applyImportTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	err := tx.execErrs[min(tx.execCalls, len(tx.execErrs)-1)]
	tx.execCalls++
	return pgconn.CommandTag{}, err
}

func TestSaveImportReturnsWriteErrors(t *testing.T) {
	writeErr := errors.New("billing import write unavailable")
	syncErr := errors.New("billing import revision sync unavailable")
	credentialErr := errors.New("billing import credential removal unavailable")
	existing := &ImportState{Source: SourceOpenAI, Enabled: true, Scope: json.RawMessage(`{}`)}
	credential := "33333333-3333-3333-3333-333333333333"
	for _, tc := range []struct {
		name     string
		state    *ImportState
		execErrs []error
		want     error
	}{
		{"insert", nil, []error{writeErr}, writeErr},
		{"update", existing, []error{writeErr}, writeErr},
		{"insert revision", nil, []error{nil, syncErr}, syncErr},
		{"update revision", existing, []error{nil, syncErr}, syncErr},
		{"credential removal", &ImportState{Source: SourceOpenAI, Enabled: true, Scope: json.RawMessage(`{}`), CredentialID: &credential}, []error{nil, credentialErr}, credentialErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &applyImportTx{row: applyImportRow{state: tc.state}, execErrs: tc.execErrs}
			_, err := SaveImport(t.Context(), tx, "11111111-1111-1111-1111-111111111111", ImportConfiguration{Source: SourceOpenAI, Enabled: true}, "22222222-2222-2222-2222-222222222222")
			if !errors.Is(err, tc.want) {
				t.Fatalf("SaveImport error = %v, want %v", err, tc.want)
			}
		})
	}
}
