package providers

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

// Pool edits keep their collection precondition; individual edits can use the
// slot precondition without borrowing authority over unrelated pool changes.
func matchSlot(r *http.Request, current *record, slot *slotRow) error {
	if r.Header.Get("If-Match") == `"`+current.SlotsETag+`"` || slot == nil {
		return access.Match(r, current.SlotsETag)
	}
	return access.Match(r, slot.ETag)
}

func (s *Server) getSlot(r *http.Request, p access.Principal) (access.Reply, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = visibleProvider(r.Context(), s.Access.Pool, p, providerID); err != nil {
		return access.Reply{}, err
	}
	rows, err := loadSlots(r.Context(), s.Access.Pool, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	slot := findSlot(rows, slotID)
	if slot == nil {
		return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
	}
	body := slotJSON(*slot)
	body["provider_id"], body["is_default"], body["etag"] = providerID, slot.Default, slot.ETag
	return access.Detail(body, slot.ETag), nil
}

func (s *Server) deleteSlot(r *http.Request, _ access.Principal) (access.Reply, error) {
	providerID, err := access.IDParam(r, "provider_id")
	if err != nil {
		return access.Reply{}, err
	}
	slotID, err := access.IDParam(r, "slot_id")
	if err != nil {
		return access.Reply{}, err
	}
	input := map[string]string{"provider_id": providerID, "slot_id": slotID}
	return access.Mutate(s.Access, r, input, changeable(r.Context(), providerID), func(tx pgx.Tx, p access.Principal, current *record) (access.Reply, error) {
		rows, err := loadSlots(r.Context(), tx, providerID)
		if err != nil {
			return access.Reply{}, err
		}
		slot := findSlot(rows, slotID)
		if slot == nil {
			return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
		}
		if err = matchSlot(r, current, slot); err != nil {
			return access.Reply{}, err
		}
		etag, err := s.deleteDraftSlot(r.Context(), tx, providerID, slotID, slot)
		if err != nil {
			return access.Reply{}, err
		}
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.slot.delete", "provider_slot", slotID, "success"); err != nil {
			return access.Reply{}, err
		}
		return access.Reply{Status: http.StatusNoContent, ParentMutation: &access.ParentMutation{Previous: current.ETag, Current: etag}}, nil
	})
}

func (s *Catalogue) deleteDraftSlot(ctx context.Context, tx pgx.Tx, providerID, slotID string, slot *slotRow) (string, error) {
	var err error
	if slot.Default {
		return "", access.Fail(409, "default_slot_required", "The default credential slot cannot be deleted.")
	}
	// Immutable credential versions and published revision slots survive draft
	// removal. Only pending enrollment state belongs exclusively to this slot.
	if _, err = tx.Exec(ctx, `DELETE FROM olp.secrets WHERE purpose=$3 AND id IN (SELECT id FROM olp.grant_enrollments WHERE slot_id=$1 AND provider_id=$2)`, slotID, providerID, secrets.GrantEnrollment); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "DELETE FROM olp.provider_slots WHERE id=$1 AND provider_id=$2", slotID, providerID); err != nil {
		return "", err
	}
	etag, err := touch(ctx, tx, providerID)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", providerID, access.NewID()); err != nil {
		return "", err
	}
	return etag, nil
}
