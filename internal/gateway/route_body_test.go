package gateway

import (
	"testing"
)

func routeBodyLimit(t *testing.T, rt *fakeRuntime, slug string, limit int64) {
	t.Helper()
	route := rt.release.Snapshot.Routes[slug]
	route.MaxBodyBytes = &limit
	rt.release.Snapshot.Routes[slug] = route
	if err := rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
}
