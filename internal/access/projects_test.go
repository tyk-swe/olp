package access

import (
	"errors"
	"slices"
	"testing"
)

// TestProjectScopeTruthTable pins every project decision: allowed (0), 404 for
// a resource outside the caller's scope, or 403 for a visible one it cannot
// change.
func TestProjectScopeTruthTable(t *testing.T) {
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
		need      Need
		want      int
	}{
		{"global nil view", global, nil, View, 0},
		{"global nil change", global, nil, Change, 0},
		{"global project change", global, &projectA, Change, 0},
		{"manager nil", manager, nil, View, 404},
		{"manager own view", manager, &projectA, View, 0},
		{"manager own change", manager, &projectA, Change, 0},
		{"manager other", manager, &projectB, View, 404},
		{"manager other change", manager, &projectB, Change, 404},
		{"viewer nil", viewer, nil, View, 404},
		{"viewer own view", viewer, &projectA, View, 0},
		{"viewer own change", viewer, &projectA, Change, 403},
		{"viewer other", viewer, &projectB, View, 404},
		{"machine all nil", machineAll, nil, Change, 0},
		{"machine all project", machineAll, &projectB, Change, 0},
		{"machine scoped own", machineScoped, &projectA, Change, 0},
		{"machine scoped nil", machineScoped, nil, View, 404},
		{"machine scoped other", machineScoped, &projectB, Change, 404},
	} {
		got := 0
		if err := tc.principal.Project(tc.project, tc.need); err != nil {
			problem, ok := errors.AsType[*Problem](err)
			if !ok {
				t.Fatalf("%s: %v is not a problem", tc.name, err)
			}
			got = problem.Status
		}
		if got != tc.want {
			t.Fatalf("%s: Project = %d, want %d", tc.name, got, tc.want)
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
