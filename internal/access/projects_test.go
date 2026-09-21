package access

import (
	"slices"
	"testing"
)

func TestPrincipalCanProjectMatrix(t *testing.T) {
	projectA, projectB := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	global := Principal{Kind: "user", AllProjects: true}
	manager := Principal{Kind: "user", Projects: map[string]string{projectA: "manager"}}
	viewer := Principal{Kind: "user", Projects: map[string]string{projectA: "viewer"}}
	machineAll := Principal{Kind: "machine", AllProjects: true}
	machineScoped := Principal{Kind: "machine", Projects: map[string]string{projectA: "manager"}}

	for _, tc := range []struct {
		name      string
		principal Principal
		project   *string
		write     bool
		want      bool
	}{
		{"global nil read", global, nil, false, true},
		{"global nil write", global, nil, true, true},
		{"global project write", global, &projectA, true, true},
		{"manager nil", manager, nil, false, false},
		{"manager own read", manager, &projectA, false, true},
		{"manager own write", manager, &projectA, true, true},
		{"manager other", manager, &projectB, false, false},
		{"viewer nil", viewer, nil, false, false},
		{"viewer own read", viewer, &projectA, false, true},
		{"viewer own write", viewer, &projectA, true, false},
		{"viewer other", viewer, &projectB, false, false},
		{"machine all nil", machineAll, nil, true, true},
		{"machine all project", machineAll, &projectB, true, true},
		{"machine scoped own", machineScoped, &projectA, true, true},
		{"machine scoped nil", machineScoped, nil, false, false},
		{"machine scoped other", machineScoped, &projectB, true, false},
	} {
		if got := tc.principal.CanProject(tc.project, tc.write); got != tc.want {
			t.Fatalf("%s: CanProject = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPrincipalProjectIDsSorted(t *testing.T) {
	p := Principal{Projects: map[string]string{
		"cccccccc-cccc-cccc-cccc-cccccccccccc": "viewer",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa": "manager",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb": "viewer",
	}}
	got := p.ProjectIDs()
	want := []string{
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ProjectIDs = %v, want %v", got, want)
	}
	if ids := (Principal{}).ProjectIDs(); len(ids) != 0 {
		t.Fatal("an unscoped principal must report no project ids")
	}
}

func TestAssignedScopeDeniesInstallationOperations(t *testing.T) {
	assigned := Principal{Kind: "user", Projects: map[string]string{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa": "manager"}}
	for _, operation := range []string{"access", "access_read", "settings"} {
		if err := restrictInstallation(assigned, operation); err == nil {
			t.Fatalf("assigned principal must be denied the %s operation", operation)
		}
	}
	for _, operation := range []string{"read", "configure", "keys", "playground", "usage"} {
		if err := restrictInstallation(assigned, operation); err != nil {
			t.Fatalf("assigned principal must keep the %s operation: %v", operation, err)
		}
	}
	global := Principal{Kind: "user", AllProjects: true}
	for _, operation := range []string{"access", "access_read", "settings"} {
		if err := restrictInstallation(global, operation); err != nil {
			t.Fatalf("global principal must keep the %s operation", operation)
		}
	}
}

func TestPermissionGrantsUsageWhereReadWasAllowed(t *testing.T) {
	for _, role := range []string{"owner", "operator", "developer", "viewer"} {
		if !Permission(role, "usage") {
			t.Fatalf("role %s must retain usage visibility", role)
		}
	}
}
