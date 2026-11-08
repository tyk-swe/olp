package retrievalbinding

import (
	"context"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/retrieval"
)

func Applier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		if len(desired.Bindings) == 0 {
			return nil
		}
		directory, err := project.ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		for _, binding := range desired.Bindings {
			if err := apply(ctx, tx, p, directory, binding); err != nil {
				return err
			}
		}
		_, err = access.AdvanceAuthority(ctx, tx)
		return err
	}
}

func apply(ctx context.Context, tx pgx.Tx, p access.Principal, directory project.Directory, binding Binding) error {
	in, err := resolve(ctx, tx, directory, binding)
	if err != nil {
		return err
	}
	if err := p.Authorize(access.Configure); err != nil {
		return err
	}
	if err := p.Project(&in.ProjectID, access.Change); err != nil {
		return err
	}
	current, err := locked(ctx, tx, in)
	if err != nil {
		return err
	}
	if current != nil && reflect.DeepEqual(current.Input, in) {
		return nil
	}
	_, err = retrieval.Save(ctx, tx, p.UserID(), current, in)
	return err
}

func locked(ctx context.Context, tx pgx.Tx, in retrieval.Input) (*retrieval.Binding, error) {
	current, err := find(ctx, tx, in)
	if err != nil || current == nil {
		return nil, err
	}
	return retrieval.Load(ctx, tx, current.ID, true)
}
