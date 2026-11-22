package codesessions

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *Store) bind(ctx context.Context, tx pgx.Tx, in Admission) (codemode.Binding, *codeSelection, error) {
	identity := in.Operation.Identity
	b, err := codeConversation(ctx, tx, in, identity.Conversation)
	if err == nil {
		if err = liveCodeTree(ctx, tx, b); err != nil {
			return b, nil, err
		}
		if identity.Parent != "" {
			var parent string
			if err = tx.QueryRow(ctx, `SELECT conversation FROM olp.code_bindings WHERE id=$1`, b.ParentID).Scan(&parent); err != nil || parent != identity.Parent {
				return b, nil, codemode.Refuse(409, "code_parent_conflict")
			}
		}
		return b, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return b, nil, err
	}
	b = codemode.Binding{ID: httpkit.NewID(), ProjectID: in.Route.ProjectID, RouteID: in.Route.ID, APIKeyID: in.APIKeyID, Conversation: identity.Conversation}
	b.RootID = b.ID
	var root *codeSelection
	if identity.Parent != "" {
		parent, err := codeConversation(ctx, tx, in, identity.Parent)
		if errors.Is(err, pgx.ErrNoRows) {
			return b, nil, codemode.Refuse(409, "code_parent_unresolved")
		}
		if err != nil {
			return b, nil, err
		}
		if err = liveCodeTree(ctx, tx, parent); err != nil {
			return b, nil, err
		}
		b.ParentID = &parent.ID
		b.RootID = parent.RootID
		b.AccountID = parent.AccountID
		b.Principal = parent.Principal
	} else {
		selection, err := codeAccount(ctx, tx, in, "", "", nil, in.Route.AllowanceOrdering)
		if err != nil {
			if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && refusal.Code == "code_account_unavailable" && in.Operation.Name == "connect" && len(in.Route.Models) > 1 {
				type hintResult struct {
					selection codeSelection
					err       error
				}
				hints := slices.Collect(func(yield func(hintResult) bool) {
					for _, model := range in.Route.Models {
						hinted := in.withOperation(codemode.Operation{Name: "connect_model", Model: model, Identity: in.Operation.Identity})
						candidate, hintErr := codeAccount(ctx, tx, hinted, "", "", nil, in.Route.AllowanceOrdering)
						if !yield(hintResult{selection: candidate, err: hintErr}) || hintErr == nil {
							return
						}
						if _, ok := errors.AsType[*codemode.Refusal](hintErr); !ok {
							return
						}
					}
				})
				for _, hint := range hints {
					if hint.err == nil {
						return b, nil, codemode.Refuse(400, "code_connection_model_required")
					}
					if _, ok := errors.AsType[*codemode.Refusal](hint.err); !ok {
						return b, nil, hint.err
					}
				}
				if len(selection.candidates) == 0 {
					deadlines := slices.Collect(func(yield func(*time.Time) bool) {
						for _, hint := range hints {
							if len(hint.selection.candidates) == 0 {
								continue
							}
							refusal, _ := errors.AsType[*codemode.Refusal](hint.err)
							if !yield(refusal.RetryAt) {
								return
							}
						}
					})
					if len(deadlines) > 0 {
						return b, nil, &codemode.Refusal{Status: 400, Code: "code_connection_model_required", RetryAt: codemode.EarliestRecovery(time.Now(), deadlines...)}
					}
				}
			}
			return b, nil, err
		}
		root = &selection
		b.AccountID = selection.account.ID
		b.Principal = selection.account.Principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_bindings(id,project_id,route_id,api_key_id,conversation,parent_id,root_id,account_id,principal,socket_bound)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING created_at`, b.ID, b.ProjectID, b.RouteID, b.APIKeyID, b.Conversation, b.ParentID, b.RootID, b.AccountID, b.Principal, in.Operation.Name == "connect" || in.Operation.Name == "connect_model").Scan(&b.CreatedAt)
	return b, root, err
}

func codeConversation(ctx context.Context, tx pgx.Tx, in Admission, conversation string) (codemode.Binding, error) {
	return scanBinding(tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM olp.code_bindings b WHERE b.route_id=$1 AND b.api_key_id=$2 AND b.conversation=$3`, in.Route.ID, in.APIKeyID, conversation))
}

// liveCodeTree refuses a binding whose tree is retired, and holds its root
// against retirement until the transaction ends.
func liveCodeTree(ctx context.Context, tx pgx.Tx, b codemode.Binding) error {
	var retired bool
	if err := tx.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM olp.code_bindings WHERE id=$1 FOR SHARE`, b.RootID).Scan(&retired); err != nil {
		return err
	}
	if retired || b.RetiredAt != nil {
		return codemode.Refuse(410, "code_binding_retired")
	}
	return nil
}

// codePins returns the pins of a conversation tree in the order it made them.
func codePins(ctx context.Context, tx pgx.Tx, root string) ([]codemode.Pin, error) {
	rows, err := tx.Query(ctx, `SELECT model,coalesce(target_model,model),account_id::text,principal,created_at FROM olp.code_pins WHERE root_id=$1 ORDER BY seq`, root)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (codemode.Pin, error) {
		var p codemode.Pin
		err := row.Scan(&p.Model, &p.TargetModel, &p.AccountID, &p.Principal, &p.CreatedAt)
		return p, err
	})
}

// codeTree lists a conversation tree's accounts, its own first and then its
// pins' in the order it made them, with the principal each serves it as.
func codeTree(b codemode.Binding, pins []codemode.Pin) ([]string, map[string]string) {
	tree := []string{b.AccountID}
	principals := map[string]string{b.AccountID: b.Principal}
	for _, p := range pins {
		if _, ok := principals[p.AccountID]; !ok {
			tree = append(tree, p.AccountID)
			principals[p.AccountID] = p.Principal
		}
	}
	return tree, principals
}

// codePin pins an admission's model, which the binding's tree has not served
// yet, to the account that serves it. A tree keeps one account of each
// adapter, so the model goes to the tree's own account when that serves it,
// else to another account the tree uses, else to the first available account
// of an adapter the tree does not use yet. Nothing fails over: a tree account
// that cools refuses a new model it serves as it refuses its pinned ones.
func codePin(ctx context.Context, tx pgx.Tx, in Admission, b codemode.Binding, pins []codemode.Pin, bandOrdered bool) (codemode.Pin, codeSelection, error) {
	tree, principals := codeTree(b, pins)
	selection, err := codeAccount(ctx, tx, in, "", b.RootID, tree, bandOrdered)
	if err != nil {
		return codemode.Pin{}, selection, err
	}
	account := selection.account
	pin := codemode.Pin{Model: in.Operation.PublicModel(), TargetModel: in.Operation.Model, AccountID: account.ID, Principal: account.Principal}
	if principal, ok := principals[account.ID]; ok {
		pin.Principal = principal
	}
	err = tx.QueryRow(ctx, `INSERT INTO olp.code_pins(root_id,model,account_id,principal,adapter,target_model)
		SELECT $1,$2,$3,$4,`+codeadapter.SQL("v.connections->($5::text)")+`,$7 FROM olp.code_route_revisions v WHERE v.id=$6 RETURNING created_at`,
		b.RootID, pin.Model, pin.AccountID, pin.Principal, account.ProviderID, in.Route.RevisionID, pin.TargetModel).Scan(&pin.CreatedAt)
	return pin, selection, err
}
