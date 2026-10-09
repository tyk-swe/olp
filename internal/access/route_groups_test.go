package access

import (
	"testing"
	"time"
)

func TestRouteGroupsCompileWithoutWideningKeyScope(t *testing.T) {
	project := "project"
	other := "other"
	a := Authority{ProjectID: &project, Policy: KeyPolicy{Scopes: []string{"inference", "models_read"}, AllowedRouteGroups: []string{"production"}, AllowedRoutes: []string{"explicit"}}}
	groups := RouteGroups{"production": {"chat", "coding"}, "empty": {}}
	if err := a.BindRouteGroups(groups); err != nil {
		t.Fatal(err)
	}
	groups["production"][0] = "changed"
	for _, scope := range []string{"inference", "models_read"} {
		for _, route := range []string{"chat", "coding", "explicit"} {
			if !a.Allows(scope, route, &project, time.Now()) {
				t.Fatalf("missing union grant: %s %s", scope, route)
			}
		}
		if a.Allows(scope, "chat", &other, time.Now()) || a.Allows(scope, "changed", &project, time.Now()) {
			t.Fatal("scope widened or cached membership mutated")
		}
	}
	a.Policy.AllowedRoutes = nil
	for _, group := range []string{"empty", "removed"} {
		a.Policy.AllowedRouteGroups = []string{group}
		if err := a.BindRouteGroups(groups); err != nil {
			t.Fatal(err)
		}
		if a.Allows("inference", "chat", &project, time.Now()) {
			t.Fatal("empty or missing group widened access")
		}
	}
	if list := a.RouteAllowlist(); list == nil || len(list) != 0 {
		t.Fatalf("empty group collection filter: %v", list)
	}
	a.Policy.AllowedRouteGroups = nil
	if a.RouteAllowlist() != nil {
		t.Fatal("unrestricted collection filter is not nil")
	}
	if !a.Allows("inference", "chat", &project, time.Now()) {
		t.Fatal("empty lists must retain unrestricted project semantics")
	}
	if got := testing.AllocsPerRun(100, func() { a.Allows("inference", "chat", &project, time.Now()) }); got != 0 {
		t.Fatalf("unconfigured allocations: %g", got)
	}
}
func TestRouteGroupsValidateNamesAndSets(t *testing.T) {
	for _, groups := range []RouteGroups{{"Bad name": {"chat"}}, {"valid": nil}, {"valid": {"chat", "chat"}}, {"valid": {"../chat"}}} {
		if groups.Validate() == nil {
			t.Fatalf("invalid groups accepted: %v", groups)
		}
	}
	if err := (RouteGroups{"empty": {}, "valid": {"future", "coding"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if !(RouteGroups{"set": {"one", "two"}}).Equal(RouteGroups{"set": {"two", "one"}}) {
		t.Fatal("sets depend on order")
	}
}
