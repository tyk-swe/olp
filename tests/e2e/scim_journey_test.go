//go:build e2e

package e2e_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

func TestDirectoryProvisionsSearchesAndRemovesUsers(t *testing.T) {
	t.Parallel()
	s := startService(t)
	directory := s.installationToken(s.owner(), "directory")
	provision := func(name string, emails ...any) string {
		return s.want(directory, "POST", "/scim/v2/Users", map[string]any{
			"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": name + "@example.test", "displayName": name, "emails": emails,
		}, nil, http.StatusCreated)["id"].(string)
	}
	ada := provision("ada",
		map[string]any{"value": "ada@work.example.test", "type": "work", "primary": true},
		map[string]any{"value": "ada@home.example.test", "type": "home"})
	provision("grace")
	linus := provision("linus")
	search := func(parameters url.Values) (float64, []string) {
		page := s.want(directory, "GET", "/scim/v2/Users?"+parameters.Encode(), nil, nil, http.StatusOK)
		names := []string{}
		for _, resource := range items(page["Resources"]) {
			names = append(names, text(resource, "userName"))
		}
		return page["totalResults"].(float64), names
	}

	if total, names := search(url.Values{"filter": {`userName eq "ADA@EXAMPLE.TEST"`}}); total != 1 || !slices.Equal(names, []string{"ada@example.test"}) {
		t.Errorf("a case-insensitive userName filter found %v of %v", names, total)
	}
	if total, names := search(url.Values{"sortBy": {"userName"}, "sortOrder": {"descending"}, "startIndex": {"2"}, "count": {"2"}}); total != 3 || !slices.Equal(names, []string{"grace@example.test", "ada@example.test"}) {
		t.Errorf("the page after the first user, sorted descending, held %v of %v", names, total)
	}

	patch := func(id string, operation map[string]any) map[string]any {
		return s.want(directory, "PATCH", "/scim/v2/Users/"+id, map[string]any{
			"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"}, "Operations": []any{operation},
		}, nil, http.StatusOK)
	}
	moved := patch(ada, map[string]any{"op": "replace", "path": `emails[type eq "home"].value`, "value": "ada@new.example.test"})
	addresses := map[string]string{}
	for _, email := range items(moved["emails"]) {
		addresses[text(email, "type")] = text(email, "value")
	}
	if addresses["home"] != "ada@new.example.test" || addresses["work"] != "ada@work.example.test" {
		t.Errorf("replacing the home address left %v", addresses)
	}
	patch(ada, map[string]any{"op": "replace", "path": "active", "value": false})
	if _, names := search(url.Values{"filter": {"active eq false"}}); !slices.Equal(names, []string{"ada@example.test"}) {
		t.Errorf("after deactivating one user, inactive users are %v", names)
	}

	s.want(directory, "DELETE", "/scim/v2/Users/"+linus, nil, nil, http.StatusNoContent)
	s.want(directory, "GET", "/scim/v2/Users/"+linus, nil, nil, http.StatusNotFound)
	if total, names := search(url.Values{}); total != 2 || slices.Contains(names, "linus@example.test") {
		t.Errorf("after removing a user the directory lists %v of %v", names, total)
	}
}
