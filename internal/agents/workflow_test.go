package agents

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestWorkflowRequiresBoundProjectAndRouteRevision(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	key := secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), project)
	store := &WorkflowStore{Pool: &pgxpool.Pool{}, Auth: key}
	for _, test := range []struct {
		name      string
		store     *WorkflowStore
		authority access.Authority
		route     string
		revision  string
	}{
		{name: "missing store", authority: access.Authority{ProjectID: &project}, route: project, revision: project},
		{name: "missing authority", store: store, route: project, revision: project},
		{name: "missing route", store: store, authority: access.Authority{ProjectID: &project}, revision: project},
		{name: "invalid route", store: store, authority: access.Authority{ProjectID: &project}, route: "invalid", revision: project},
		{name: "missing revision", store: store, authority: access.Authority{ProjectID: &project}, route: project},
		{name: "invalid revision", store: store, authority: access.Authority{ProjectID: &project}, route: project, revision: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			id, err := test.store.Start(context.Background(), test.authority, test.route, test.revision)
			if id != "" || !errors.Is(err, limits.ErrDebitAuthority) {
				t.Fatalf("unbound workflow returned id %q and error %v", id, err)
			}
		})
	}
}
