package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

func decodeSCIMDoc(t *testing.T, raw string) scim.Document {
	t.Helper()
	doc, err := scim.Decode(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decode SCIM doc: %v", err)
	}
	return doc
}

func scimErrType(err error) (int, string) {
	if e, ok := errors.AsType[*scim.Error](err); ok {
		return e.Status, e.Type
	}
	return 0, ""
}

func TestParseSCIMUserDocument(t *testing.T) {
	t.Run("minimal user applies defaults and omits optional fields", func(t *testing.T) {
		doc := decodeSCIMDoc(t, `{"schemas":["`+scim.UserSchema+`"],"userName":"Ada@Example.COM"}`)
		parsed, err := parseSCIMUserDocument(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parsed.address != "ada@example.com" || parsed.display != "ada@example.com" || !parsed.active {
			t.Fatalf("unexpected parsed fields: %+v", parsed)
		}
		if parsed.extension.Role != "viewer" || parsed.extension.AccessScope != "assigned" {
			t.Fatalf("unexpected default extension: %+v", parsed.extension)
		}
		raw, err := json.Marshal(parsed.stored)
		if err != nil {
			t.Fatalf("marshal stored user: %v", err)
		}
		var out map[string]any
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshal stored user: %v", err)
		}
		for _, absent := range []string{"externalId", "name", "emails"} {
			if _, exists := out[absent]; exists {
				t.Errorf("expected %q to be omitted from stored JSON: %s", absent, raw)
			}
		}
		if _, exists := out[scim.UserExtension]; !exists {
			t.Errorf("expected %q in stored JSON: %s", scim.UserExtension, raw)
		}
	})

	t.Run("displayName falls back to formatted name or truncated email", func(t *testing.T) {
		doc := decodeSCIMDoc(t, `{"schemas":["`+scim.UserSchema+`"],"userName":"ada@example.com","name":{"formatted":"Ada Lovelace","givenName":"Ada"}}`)
		parsed, err := parseSCIMUserDocument(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parsed.display != "Ada Lovelace" {
			t.Fatalf("got display %q, want Ada Lovelace", parsed.display)
		}

		longLocal := strings.Repeat("a", 60)
		longEmail := longLocal + "@" + strings.Repeat("b", 50) + ".example"
		longDoc := decodeSCIMDoc(t, `{"schemas":["`+scim.UserSchema+`"],"userName":"`+longEmail+`"}`)
		longParsed, err := parseSCIMUserDocument(longDoc)
		if err != nil {
			t.Fatalf("unexpected error for long email: %v", err)
		}
		if len([]rune(longParsed.display)) != 100 {
			t.Fatalf("got display length %d, want 100", len([]rune(longParsed.display)))
		}
	})

	t.Run("preserves explicit empty emails array and full extension", func(t *testing.T) {
		doc := decodeSCIMDoc(t, `{
			"schemas":["`+scim.UserSchema+`"],
			"userName":"ada@example.com",
			"externalId":"dir-42",
			"displayName":"Ada",
			"active":false,
			"emails":[],
			"`+scim.UserExtension+`":{"role":"operator","accessScope":"global"}
		}`)
		parsed, err := parseSCIMUserDocument(doc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parsed.active || parsed.external != "dir-42" || parsed.extension.Role != "operator" || parsed.extension.AccessScope != "global" {
			t.Fatalf("unexpected parsed user: %+v", parsed)
		}
		raw, err := json.Marshal(parsed.stored)
		if err != nil {
			t.Fatalf("marshal stored user: %v", err)
		}
		if !strings.Contains(string(raw), `"emails":[]`) {
			t.Fatalf("expected explicit empty emails array in %s", raw)
		}
	})

	t.Run("rejects invalid documents", func(t *testing.T) {
		cases := []struct {
			name   string
			raw    string
			status int
			typ    string
		}{
			{"missing schema", `{"schemas":["` + scim.GroupSchema + `"],"userName":"ada@example.com"}`, 400, "invalidValue"},
			{"password forbidden", `{"schemas":["` + scim.UserSchema + `"],"userName":"ada@example.com","password":"secret"}`, 400, "mutability"},
			{"invalid email userName", `{"schemas":["` + scim.UserSchema + `"],"userName":"not-an-email"}`, 400, "invalidValue"},
			{"control char in displayName", `{"schemas":["` + scim.UserSchema + `"],"userName":"ada@example.com","displayName":"Ada\u0007"}`, 400, "invalidValue"},
			{"non-object name", `{"schemas":["` + scim.UserSchema + `"],"userName":"ada@example.com","name":"Ada"}`, 400, "invalidValue"},
			{"control char in name subattribute", `{"schemas":["` + scim.UserSchema + `"],"userName":"ada@example.com","name":{"givenName":"Ada\n"}}`, 400, "invalidValue"},
			{"non-boolean active", `{"schemas":["` + scim.UserSchema + `"],"userName":"ada@example.com","active":"yes"}`, 400, "invalidValue"},
		}
		for _, tc := range cases {
			_, err := parseSCIMUserDocument(decodeSCIMDoc(t, tc.raw))
			status, typ := scimErrType(err)
			if status != tc.status || typ != tc.typ {
				t.Errorf("%s: got (%d, %q), want (%d, %q) (err=%v)", tc.name, status, typ, tc.status, tc.typ, err)
			}
		}
	})
}

func TestParseSCIMUserEmails(t *testing.T) {
	t.Run("validates up to 8 emails and preserves primary tri-state", func(t *testing.T) {
		input := make([]any, 0, 8)
		for i := range 8 {
			entry := map[string]any{"value": "User" + string(rune('a'+i)) + "@Example.com", "type": "work"}
			switch i {
			case 0:
				entry["primary"] = true
			case 1:
				entry["primary"] = false
			}
			input = append(input, entry)
		}
		emails, err := parseSCIMUserEmails(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(emails) != 8 {
			t.Fatalf("got %d emails, want 8", len(emails))
		}
		if emails[0].Value != "usera@example.com" || emails[0].Primary == nil || !*emails[0].Primary {
			t.Fatalf("first email mismatch: %+v", emails[0])
		}
		if emails[1].Primary == nil || *emails[1].Primary {
			t.Fatalf("second email should preserve explicit primary=false: %+v", emails[1])
		}
		if emails[2].Primary != nil {
			t.Fatalf("third email should omit primary: %+v", emails[2])
		}
		encoded, err := json.Marshal(emails[:3])
		if err != nil {
			t.Fatalf("marshal emails: %v", err)
		}
		if want := `[{"value":"usera@example.com","type":"work","primary":true},{"value":"userb@example.com","type":"work","primary":false},{"value":"userc@example.com","type":"work"}]`; string(encoded) != want {
			t.Fatalf("encoded = %s, want %s", encoded, want)
		}
	})

	t.Run("rejects invalid email lists", func(t *testing.T) {
		tooMany := make([]any, 9)
		for i := range tooMany {
			tooMany[i] = map[string]any{"value": "u" + string(rune('a'+i)) + "@example.com"}
		}
		cases := []struct {
			name string
			raw  any
		}{
			{"non-slice", "ada@example.com"},
			{"more than 8", tooMany},
			{"non-object entry", []any{"ada@example.com"}},
			{"two primary emails", []any{
				map[string]any{"value": "a@example.com", "primary": true},
				map[string]any{"value": "b@example.com", "primary": true},
			}},
			{"non-bool primary", []any{
				map[string]any{"value": "a@example.com", "primary": "true"},
			}},
			{"control char in value", []any{
				map[string]any{"value": "a@example.com\x00"},
			}},
			{"control char in type", []any{
				map[string]any{"value": "a@example.com", "type": "work\t"},
			}},
			{"invalid email address", []any{
				map[string]any{"value": "not-an-email"},
			}},
			{"missing value", []any{
				map[string]any{"type": "work"},
			}},
		}
		for _, tc := range cases {
			_, err := parseSCIMUserEmails(tc.raw)
			status, typ := scimErrType(err)
			if status != 400 || typ != "invalidValue" {
				t.Errorf("%s: got (%d, %q), want (400, invalidValue)", tc.name, status, typ)
			}
		}
	})
}

func TestSCIMExtension(t *testing.T) {
	t.Run("user defaults role to viewer and scope to assigned", func(t *testing.T) {
		ext, err := scimExtension(scim.Document{}, scim.UserExtension, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ext.Role != "viewer" || ext.AccessScope != "assigned" {
			t.Fatalf("got %+v, want viewer/assigned", ext)
		}
	})

	t.Run("group keeps empty role when omitted", func(t *testing.T) {
		ext, err := scimExtension(scim.Document{}, scim.GroupExtension, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ext.Role != "" || ext.AccessScope != "assigned" {
			t.Fatalf("got %+v, want empty role / assigned", ext)
		}
	})

	t.Run("accepts explicit valid roles and scopes", func(t *testing.T) {
		for _, role := range []string{"owner", "operator", "developer", "viewer"} {
			doc := scim.Document{scim.GroupExtension: map[string]any{"role": role, "accessScope": "global"}}
			ext, err := scimExtension(doc, scim.GroupExtension, false)
			if err != nil {
				t.Fatalf("role %q: unexpected error: %v", role, err)
			}
			if ext.Role != role || ext.AccessScope != "global" {
				t.Fatalf("role %q: got %+v", role, ext)
			}
		}
	})

	t.Run("rejects malformed extension values", func(t *testing.T) {
		cases := []struct {
			name string
			doc  scim.Document
		}{
			{"non-object extension", scim.Document{scim.UserExtension: "viewer"}},
			{"unknown role", scim.Document{scim.UserExtension: map[string]any{"role": "superadmin"}}},
			{"unknown accessScope", scim.Document{scim.UserExtension: map[string]any{"accessScope": "org"}}},
			{"control char in role", scim.Document{scim.UserExtension: map[string]any{"role": "viewer\n"}}},
			{"non-string scope", scim.Document{scim.UserExtension: map[string]any{"accessScope": 1}}},
		}
		for _, tc := range cases {
			_, err := scimExtension(tc.doc, scim.UserExtension, true)
			status, typ := scimErrType(err)
			if status != 400 || typ != "invalidValue" {
				t.Errorf("%s: got (%d, %q), want (400, invalidValue)", tc.name, status, typ)
			}
		}
	})
}

func TestSCIMListWindow(t *testing.T) {
	valid := []struct {
		name      string
		values    url.Values
		wantStart int
		wantCount int
	}{
		{"defaults", url.Values{}, 1, 100},
		{"explicit valid", url.Values{"startIndex": {"15"}, "count": {"50"}}, 15, 50},
		{"clamps startIndex below 1", url.Values{"startIndex": {"0"}}, 1, 100},
		{"clamps negative startIndex", url.Values{"startIndex": {"-5"}}, 1, 100},
		{"clamps negative count to 0", url.Values{"count": {"-10"}}, 1, 0},
		{"clamps count above 200", url.Values{"count": {"999"}}, 1, 200},
		{"max supported startIndex", url.Values{"startIndex": {"10000000"}}, 10000000, 100},
	}
	for _, tc := range valid {
		start, count, err := scimListWindow(tc.values)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
			continue
		}
		if start != tc.wantStart || count != tc.wantCount {
			t.Errorf("%s: got (%d, %d), want (%d, %d)", tc.name, start, count, tc.wantStart, tc.wantCount)
		}
	}

	invalid := []url.Values{
		{"startIndex": {"abc"}},
		{"count": {"abc"}},
		{"startIndex": {"10000001"}},
	}
	for _, values := range invalid {
		if _, _, err := scimListWindow(values); err == nil {
			t.Errorf("expected error for %v", values)
		}
	}
}

func TestSCIMFailureForAndRender(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
		wantKnown  bool
	}{
		{"scim error", scim.Fail(409, "mutability", "managed locally"), 409, "mutability", true},
		{"problem 422 maps to 400 invalidValue", httpkit.Invalid("field", "bad"), 400, "invalidValue", true},
		{"problem 412 maps to invalidVers", httpkit.Fail(412, "etag_mismatch", "stale"), 412, "invalidVers", true},
		{"problem 401 preserves status", httpkit.Fail(401, "unauthorized", "auth needed"), 401, "", true},
		{"pgx no rows maps to 404", pgx.ErrNoRows, 404, "", true},
		{"pg unique violation maps to 409 uniqueness", &pgconn.PgError{Code: "23505"}, 409, "uniqueness", true},
		{"other pg error maps to 503 unknown", &pgconn.PgError{Code: "40001"}, 503, "", false},
		{"generic error maps to 503 unknown", errors.New("boom"), 503, "", false},
	}
	for _, tc := range cases {
		got, known := scimFailureFor(tc.err)
		if got.status != tc.wantStatus || got.typ != tc.wantType || known != tc.wantKnown {
			t.Errorf("%s: got (%+v, known=%v), want status=%d type=%q known=%v", tc.name, got, known, tc.wantStatus, tc.wantType, tc.wantKnown)
		}
	}

	rec := httptest.NewRecorder()
	WriteSCIMError(rec, httpkit.Fail(401, "unauthorized", "Token required."))
	if rec.Code != 401 {
		t.Fatalf("got status %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="SCIM"` {
		t.Fatalf("got WWW-Authenticate %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/scim+json" {
		t.Fatalf("got Content-Type %q", got)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if payload["status"] != "401" || payload["detail"] != "Token required." {
		t.Fatalf("unexpected error payload: %v", payload)
	}
	if _, hasType := payload["scimType"]; hasType {
		t.Fatalf("expected scimType to be omitted when empty: %v", payload)
	}
}

func TestSCIMDiscoveryStructure(t *testing.T) {
	svc := &scimService{access: &access.Server{Origin: "https://olp.example"}}

	t.Run("ServiceProviderConfig", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil)
		reply, err := svc.scimDiscovery(req, authz.Principal{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		cfg, ok := reply.Body.(scimServiceProviderConfig)
		if !ok {
			t.Fatalf("expected scimServiceProviderConfig, got %T", reply.Body)
		}
		if !cfg.Patch.Supported || !cfg.Filter.Supported || cfg.Filter.MaxResults != 200 || cfg.Bulk.Supported || !cfg.ETag.Supported {
			t.Fatalf("unexpected provider config: %+v", cfg)
		}
		if cfg.Meta.Location != "https://olp.example/scim/v2/ServiceProviderConfig" {
			t.Fatalf("unexpected location: %q", cfg.Meta.Location)
		}
	})

	t.Run("Schemas list and lookup", func(t *testing.T) {
		listReq := httptest.NewRequest(http.MethodGet, "/scim/v2/Schemas", nil)
		listReply, err := svc.scimDiscovery(listReq, authz.Principal{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		list, ok := listReply.Body.(scimListResponse[scimSchemaDef])
		if !ok {
			t.Fatalf("expected scimListResponse[scimSchemaDef], got %T", listReply.Body)
		}
		if list.TotalResults != 4 || list.ItemsPerPage != 4 || list.StartIndex != 1 {
			t.Fatalf("unexpected list envelope: %+v", list)
		}
		ids := make([]string, 0, len(list.Resources))
		for _, schema := range list.Resources {
			ids = append(ids, schema.ID)
			if schema.Meta.Location != "https://olp.example/scim/v2/Schemas/"+schema.ID {
				t.Errorf("schema %s location = %q", schema.ID, schema.Meta.Location)
			}
		}
		wantIDs := []string{scim.UserSchema, scim.GroupSchema, scim.UserExtension, scim.GroupExtension}
		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("got schema ids %v, want %v", ids, wantIDs)
		}

		oneReq := httptest.NewRequest(http.MethodGet, "/scim/v2/Schemas/"+scim.UserSchema, nil)
		oneReq.SetPathValue("schema_id", scim.UserSchema)
		oneReply, err := svc.scimDiscovery(oneReq, authz.Principal{})
		if err != nil {
			t.Fatalf("single schema lookup error: %v", err)
		}
		userSchema, ok := oneReply.Body.(scimSchemaDef)
		if !ok || userSchema.ID != scim.UserSchema {
			t.Fatalf("unexpected single schema: %+v", oneReply.Body)
		}
		attrs := make(map[string]scimSchemaAttribute, len(userSchema.Attributes))
		for _, attr := range userSchema.Attributes {
			attrs[attr.Name] = attr
		}
		if attrs["userName"].Uniqueness != "server" || !attrs["userName"].Required {
			t.Errorf("unexpected userName attr: %+v", attrs["userName"])
		}
		if attrs["groups"].Mutability != "readOnly" || attrs["roles"].Mutability != "readOnly" {
			t.Errorf("expected groups and roles readOnly: groups=%+v roles=%+v", attrs["groups"], attrs["roles"])
		}
		if len(attrs["name"].SubAttributes) != len(scim.NameSubattributes()) {
			t.Errorf("unexpected name subAttributes count: %d", len(attrs["name"].SubAttributes))
		}

		missingReq := httptest.NewRequest(http.MethodGet, "/scim/v2/Schemas/unknown", nil)
		missingReq.SetPathValue("schema_id", "unknown")
		if _, err := svc.scimDiscovery(missingReq, authz.Principal{}); err == nil {
			t.Fatalf("expected 404 for unknown schema")
		}
	})

	t.Run("ResourceTypes list and lookup", func(t *testing.T) {
		listReq := httptest.NewRequest(http.MethodGet, "/scim/v2/ResourceTypes", nil)
		listReply, err := svc.scimDiscovery(listReq, authz.Principal{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		list, ok := listReply.Body.(scimListResponse[scimResourceType])
		if !ok || len(list.Resources) != 2 {
			t.Fatalf("unexpected resource types list: %+v", listReply.Body)
		}
		if list.Resources[0].ID != "User" || list.Resources[0].Endpoint != "/Users" || list.Resources[0].Schema != scim.UserSchema {
			t.Fatalf("unexpected User resource type: %+v", list.Resources[0])
		}
		if list.Resources[1].ID != "Group" || list.Resources[1].Endpoint != "/Groups" || list.Resources[1].Schema != scim.GroupSchema {
			t.Fatalf("unexpected Group resource type: %+v", list.Resources[1])
		}

		oneReq := httptest.NewRequest(http.MethodGet, "/scim/v2/ResourceTypes/Group", nil)
		oneReq.SetPathValue("scim_type", "Group")
		oneReply, err := svc.scimDiscovery(oneReq, authz.Principal{})
		if err != nil {
			t.Fatalf("single resource type error: %v", err)
		}
		rt, ok := oneReply.Body.(scimResourceType)
		if !ok || rt.ID != "Group" || rt.Meta.Location != "https://olp.example/scim/v2/ResourceTypes/Group" {
			t.Fatalf("unexpected Group resource type lookup: %+v", oneReply.Body)
		}

		missingReq := httptest.NewRequest(http.MethodGet, "/scim/v2/ResourceTypes/Other", nil)
		missingReq.SetPathValue("scim_type", "Other")
		if _, err := svc.scimDiscovery(missingReq, authz.Principal{}); err == nil {
			t.Fatalf("expected 404 for unknown resource type")
		}
	})
}
