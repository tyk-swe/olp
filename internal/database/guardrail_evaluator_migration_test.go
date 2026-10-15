//go:build integration

package database

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestIntegrationGuardrailEvaluatorParentMigration(t *testing.T) {
	pool := scratchPool(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name()[:4] > "0068" {
			break
		}
		path := "migrations/" + entry.Name()
		data, err := fs.ReadFile(migrations, path)
		if err != nil {
			t.Fatal(err)
		}
		previous[path] = &fstest.MapFile{Data: data}
	}
	if err := migrate(t.Context(), pool, previous); err != nil {
		t.Fatal(err)
	}
	owner, key := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag)
 VALUES($1,'guardrail-migration@example.test','Guardrail migration','owner',$1)`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag)
 VALUES($1,'guardrail-migration',decode('01','hex'),'Guardrail migration',$2,'{}',$1)`, key, owner); err != nil {
		t.Fatal(err)
	}
	insert := func(origin string, apiKey, parent, evaluator *string) error {
		_, err := pool.Exec(t.Context(), `INSERT INTO olp.requests
 (id,runtime_generation_id,api_key_id,route_slug,operation,surface,started_at,origin,parent_request_id,evaluator_guardrail_id)
 VALUES($1,$2,$3,'evaluator','generation','openai',now(),$4,$5,$6)`, uuid.NewString(), uuid.NewString(), apiKey, origin, parent, evaluator)
		return err
	}
	parent, evaluator := uuid.NewString(), uuid.NewString()
	if err := insert("playground", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := insert("playground", nil, &parent, &evaluator); err == nil {
		t.Fatal("fixture already permits evaluator parents")
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	for _, tc := range []struct {
		name, origin           string
		key, parent, evaluator *string
		valid                  bool
	}{
		{"playground", "playground", nil, nil, nil, true},
		{"playground evaluator", "playground", nil, &parent, &evaluator, true},
		{"missing evaluator", "playground", nil, &parent, nil, false},
		{"missing parent", "playground", nil, nil, &evaluator, false},
		{"caller", "caller", &key, nil, nil, true},
		{"caller parent", "caller", &key, &parent, nil, false},
		{"probe", "probe", nil, nil, nil, true},
		{"probe parent", "probe", nil, &parent, nil, false},
		{"classifier", "classifier", &key, &parent, nil, true},
		{"classifier parent", "classifier", &key, nil, nil, false},
		{"shadow", "shadow", nil, &parent, nil, true},
		{"shadow parent", "shadow", nil, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(tc.origin, tc.key, tc.parent, tc.evaluator)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "requests_origin_parent_check" {
				t.Fatalf("invalid parent relationship accepted: %v", err)
			}
		})
	}
}
