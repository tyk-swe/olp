//go:build integration

package database

import (
	"encoding/json"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
)

func TestIntegrationNativeBatchMetadataSurvivesAccountingUpgrade(t *testing.T) {
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
	owner, provider, key, forgedGroup := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'native-migration@example.test','Native migration','owner',$1)`, []any{owner}},
		{`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by) VALUES($1,'Native migration','openai','draft','{}',$1,$1,$2)`, []any{provider, owner}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag) VALUES($1,'native-migration',decode('01','hex'),'Native migration',$2,'{}',$1)`, []any{key, owner}},
	} {
		if _, err := pool.Exec(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	metadata := []map[string]any{
		{"pending_usage": map[string]any{"version": 2, "budget_group_id": "not-a-uuid", "attempts": "opaque native value"}},
		{"pending_usage": map[string]any{"version": 1, "budget_group_id": forgedGroup, "route_slug": "migration", "attempts": []any{map[string]any{"ordinal": 1, "provider_id": provider, "routing": map[string]any{}}}}},
	}
	ids := make([]string, 0, len(metadata))
	for _, meta := range metadata {
		id := uuid.NewString()
		ids = append(ids, id)
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,upstream_id,state,metadata)
 VALUES($1,'batch',$2,'migration',$3,$3,$3,$3,($1::uuid)::text,'validating',$4::jsonb)`, id, key, provider, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatalf("accounting migration interpreted opaque native metadata: %v", err)
	}
	var projection, group int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resource_pending_usage WHERE resource_id=ANY($1::uuid[])`, ids).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.budget_groups WHERE id=$1`, forgedGroup).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if projection != 0 || group != 0 {
		t.Fatalf("native fields invented trusted accounting ownership: projection=%d group=%d", projection, group)
	}
	for i, id := range ids {
		encoded, _ := json.Marshal(metadata[i])
		var unchanged, noExpiry bool
		if err := pool.QueryRow(t.Context(), `SELECT metadata=$2::jsonb,expires_at IS NULL FROM olp.provider_resources WHERE id=$1`, id, string(encoded)).Scan(&unchanged, &noExpiry); err != nil || !unchanged || !noExpiry {
			t.Fatalf("native metadata became a deferred owner: preserved=%v noExpiry=%v error=%v", unchanged, noExpiry, err)
		}
	}
}
