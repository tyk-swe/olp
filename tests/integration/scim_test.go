//go:build integration

package integration_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/scim"
	"github.com/tyk-swe/olp/tests/harness"
)

const scimDirectory = `[
	{"id":"ada","userName":"ada@example.test","externalId":"EXT-1","displayName":"Ada Lovelace","active":true,
	 "emails":[{"value":"zed@home.example.test","type":"home"},{"value":"ada@work.example.test","type":"work","primary":true}],
	 "meta":{"created":"2026-01-10T00:00:00Z"},
	 "` + scim.UserExtension + `":{"role":"owner"}},
	{"id":"grace","userName":"grace@example.test","externalId":"ext-1","active":false,
	 "emails":[{"value":"grace@home.example.test","type":"home"}],
	 "meta":{"created":"2026-03-01T00:30:00+02:00"},
	 "` + scim.UserExtension + `":{"role":"viewer"}},
	{"id":"linus","userName":"linus@example.test","externalId":"","displayName":"Linus","active":true,
	 "meta":{"created":"2026-03-05T00:00:00Z"},
	 "` + scim.UserExtension + `":{"role":"viewer"}}
]`

func TestSCIMFiltersSelectResourcesInPostgreSQL(t *testing.T) {
	t.Parallel()
	pool, _ := harness.EmptyDatabase(t)
	for filter, want := range map[string][]string{
		`userName eq "ADA@EXAMPLE.TEST"`: {"ada"},
		`userName sw "GRACE"`:            {"grace"},
		`userName ew "example"`:          {},
		`displayName eq "ada lovelace"`:  {"ada"},
		`externalId eq "ext-1"`:          {"grace"},
		`externalId co "EXT"`:            {"ada"},
		`displayName co "a.a"`:           {},
		`userName eq "x' OR '1'='1"`:     {},
		`active eq false`:                {"grace"},
		`not (userName sw "a")`:          {"grace", "linus"},
		`userName sw "ada" or userName sw "linus" and active eq false`: {"ada"},
		`displayName pr`:         {"ada", "linus"},
		`externalId pr`:          {"ada", "grace"},
		`displayName eq null`:    {"grace"},
		`externalId ne null`:     {"ada", "grace"},
		`nickName eq null`:       {},
		`emails[type eq "home"]`: {"ada", "grace"},
		`emails[type eq "work" and value ew "home.example.test"]`:                   {},
		`emails.type eq "work" and emails.value ew "home.example.test"`:             {"ada"},
		`meta.created lt "2026-02-28T23:00:00Z"`:                                    {"ada", "grace"},
		`meta.created ge "2026-03-05T01:00:00+01:00"`:                               {"linus"},
		scim.UserExtension + `:role eq "OWNER"`:                                     {"ada"},
		`emails[value co "example"] and ` + scim.UserExtension + `:role ne "owner"`: {"grace"},
	} {
		compiled := scim.SQLFilter{Schema: scim.UserSchema, Args: []any{[]byte(scimDirectory)}}
		where, err := compiled.Compile(filter, "item.value")
		if err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		var raw []byte
		if err = pool.QueryRow(t.Context(), "SELECT COALESCE(jsonb_agg(item.value->>'id' ORDER BY item.position),'[]'::jsonb) FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY item(value,position) WHERE "+where, compiled.Args...).Scan(&raw); err != nil {
			t.Errorf("%s: %v", filter, err)
			continue
		}
		var got []string
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s selected %v, want %v", filter, got, want)
		}
	}
}

func TestSCIMSortOrdersResourcesInPostgreSQL(t *testing.T) {
	t.Parallel()
	pool, _ := harness.EmptyDatabase(t)
	for attribute, want := range map[string][]string{
		"emails.value": {"ada", "grace", "linus"},
		"displayName":  {"ada", "linus", "grace"},
	} {
		compiled := scim.SQLFilter{Schema: scim.UserSchema, Args: []any{[]byte(scimDirectory)}}
		order, err := compiled.Sort(attribute, "item.value")
		if err != nil {
			t.Errorf("%s: %v", attribute, err)
			continue
		}
		var raw []byte
		if err = pool.QueryRow(t.Context(), "SELECT jsonb_agg(item.value->>'id' ORDER BY "+order+" ASC NULLS LAST) FROM jsonb_array_elements($1::jsonb) item(value)", compiled.Args...).Scan(&raw); err != nil {
			t.Errorf("%s: %v", attribute, err)
			continue
		}
		var got []string
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("sorting by %s gave %v, want %v", attribute, got, want)
		}
	}
}
