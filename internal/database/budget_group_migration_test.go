//go:build integration

package database

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
)

func TestIntegrationPendingOriginalGroupUpgradeRetainsIdentityWithoutPolicy(t *testing.T) {
	pool := scratchPool(t)
	previous := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name()[:4] > "0059" {
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
	owner, provider, key, resource, group := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'migration@example.test','Migration','owner',$1)`, []any{owner}},
		{`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'Migration','openai','draft','{}',$1,$1,$2)`, []any{provider, owner}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag) VALUES($1,'migration',decode('01','hex'),'Migration',$2,'{}',$1)`, []any{key, owner}},
		{`INSERT INTO olp.provider_resources(id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,upstream_id,state,contract_version,expires_at,metadata) VALUES($1,'interaction',$2,'migration',$3,$3,$3,$3,($1::uuid)::text,'in_progress','gemini-interaction/v1beta',now()+interval '24 hours',jsonb_build_object('pending_usage',jsonb_build_object('budget_group_id',$4::text,'route_slug','migration','attempts',jsonb_build_array(jsonb_build_object('ordinal',1,'provider_id',($3::uuid)::text,'routing','{}'::jsonb)))))`, []any{resource, key, provider, group}},
	} {
		if _, err := pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The original group was physically deleted before this old pending template
	// could emit its first receipt; neither creator nor project is recoverable.
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	var safe, projected bool
	if err := pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL AND created_by IS NULL AND project_id IS NULL AND limit_template IS NULL AND requests_per_minute IS NULL AND tokens_per_minute IS NULL AND max_concurrency IS NULL AND daily_cost_limit IS NULL AND weekly_cost_limit IS NULL AND monthly_cost_limit IS NULL FROM olp.budget_groups WHERE id=$1`, group).Scan(&safe); err != nil || !safe {
		t.Fatalf("upgrade invented group policy/provenance: %v %v", safe, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE resource_id=$1 AND budget_group_id=$2) AND NOT EXISTS(SELECT 1 FROM olp.budget_groups_with_limits WHERE id=$2)`, resource, group).Scan(&projected); err != nil || !projected {
		t.Fatalf("upgrade lost original group or published retired shell: %v %v", projected, err)
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM olp.budget_groups WHERE id=$1", group); err == nil {
		t.Fatal("projected original group has no foreign-key fence")
	}
	if _, err := pool.Exec(t.Context(), "UPDATE olp.budget_groups SET retired_at=NULL WHERE id=$1", group); err == nil {
		t.Fatal("orphan shell could become active without known provenance/policy")
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}
