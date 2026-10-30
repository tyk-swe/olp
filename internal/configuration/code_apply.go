package configuration

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
)

func applyCode(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	if doc.Code == nil {
		return nil
	}
	if err := authorizeCodePromotion(ctx, tx, p, doc); err != nil {
		return err
	}
	resolved, err := resolveCode(ctx, tx, doc.Code, doc.codeBindings)
	if err != nil {
		return err
	}
	changes, err := codeChanges(ctx, tx, resolved)
	if err != nil {
		return err
	}
	changed := func(id string) bool {
		return slices.ContainsFunc(changes, func(c codeChange) bool { return c.ID == id && c.Action != "noop" })
	}
	for _, e := range resolved.Accounts {
		if !changed(e.ID) {
			continue
		}
		if _, err := providers.SaveCodeAccount(ctx, tx, p, e.ID, access.NewID(), e.Input, e.Existing); err != nil {
			return err
		}
	}
	for _, e := range resolved.Pools {
		if !changed(e.ID) {
			continue
		}
		if _, err := access.SaveCodePool(ctx, tx, p, e.ID, access.NewID(), e.Input, e.Existing); err != nil {
			return err
		}
	}
	for _, e := range resolved.Routes {
		if !changed(e.ID) {
			continue
		}
		change := changes[slices.IndexFunc(changes, func(c codeChange) bool { return c.ID == e.ID })]
		if change.PublicationChanged && e.Input.Published != nil {
			publication := e.Input.Published
			route, err := routes.SaveCodeRoute(ctx, tx, p, e.ID, access.NewID(), publication.Policy.input(e.Input.Project, e.Input.Slug, publication.Policy.Pool), e.Existing)
			if err != nil {
				return err
			}
			if _, err := routes.SaveCodePublication(ctx, tx, p, e.ID, route.Route, maps.Collect(func(yield func(string, providers.Configuration) bool) {
				for id, c := range publication.Connections {
					if !yield(id, c.Configuration) {
						return
					}
				}
			}), publication.paidResolved); err != nil {
				return err
			}
		}
		if _, err := routes.SaveCodeRoute(ctx, tx, p, e.ID, access.NewID(), e.Input.Draft.input(e.Input.Project, e.Input.Slug, e.Input.Draft.Pool), e.Existing || change.PublicationChanged && e.Input.Published != nil); err != nil {
			return err
		}
		if change.PublicationChanged && e.Input.Published == nil {
			if _, err := tx.Exec(ctx, `UPDATE olp.code_routes SET latest_revision_id=NULL WHERE id=$1`, e.ID); err != nil {
				return err
			}
		}
	}
	for _, e := range resolved.Budgets {
		if !changed(e.ID) {
			continue
		}
		if _, err := access.SaveCodeBudget(ctx, tx, p, e.ID, access.NewID(), e.Input, e.Existing); err != nil {
			return err
		}
	}
	if slices.ContainsFunc(changes, func(c codeChange) bool { return c.Action != "noop" }) {
		_, err := runtime.Publish(ctx, tx, p.UserID())
		return err
	}
	return nil
}
