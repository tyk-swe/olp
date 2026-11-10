package pool_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/pool"
)

func TestPoolWriteRequiresProjectManagementAndValidatesOwnershipAndWeights(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	draft, err := pool.ParseDraft(pool.Input{
		ProjectID: strings.ToUpper(id), Name: "pool", Kind: code.PoolPersonal,
		OwnerUserID: new(strings.ToUpper(id)), AccountIDs: []string{strings.ToUpper(id)},
		AccountWeights: map[string]int64{id: 3}, APIKeyIDs: []string{strings.ToUpper(id)},
	})
	if err != nil || draft.ProjectID() != id || draft.AccountIDs()[0] != id {
		t.Fatalf("ParseDraft = %+v, %v", draft, err)
	}
	developer := access.Principal{Projects: map[string]string{id: "developer"}}
	var problem *access.Problem
	if _, err := pool.Save(t.Context(), nil, developer, id, id, draft, false); !errors.As(err, &problem) || problem.Status != 403 {
		t.Fatalf("developer saved pool: %v", err)
	}
	for _, tc := range []struct {
		kind  code.PoolKind
		owner *string
		valid bool
	}{
		{code.PoolShared, nil, true},
		{code.PoolPersonal, new(id), true},
		{code.PoolShared, new(id), false},
		{code.PoolPersonal, nil, false},
		{"unknown", nil, false},
	} {
		err := (pool.Input{ProjectID: id, Name: "pool", Kind: tc.kind, OwnerUserID: tc.owner, AccountIDs: []string{}, APIKeyIDs: []string{}}).Validate()
		if (err == nil) != tc.valid {
			t.Fatalf("%s/owner=%t valid=%t err=%v", tc.kind, tc.owner != nil, tc.valid, err)
		}
	}
	for _, tc := range []struct {
		weight int64
		valid  bool
	}{{0, false}, {1, true}, {2147483647, true}, {2147483648, false}} {
		t.Run(fmt.Sprintf("weight-%d", tc.weight), func(t *testing.T) {
			err := (pool.Input{ProjectID: id, Name: "pool", Kind: code.PoolShared, AccountIDs: []string{id}, APIKeyIDs: []string{}, AccountWeights: map[string]int64{id: tc.weight}}).Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("weight %d valid=%t err=%v", tc.weight, tc.valid, err)
			}
		})
	}
}
