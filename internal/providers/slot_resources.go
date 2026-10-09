package providers

import (
	"net/http"

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
	for i := range rows {
		if rows[i].ID == slotID {
			body := slotJSON(rows[i])
			body["provider_id"], body["is_default"], body["etag"] = providerID, rows[i].Default, rows[i].ETag
			return access.Detail(body, rows[i].ETag), nil
		}
	}
	return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
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
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, err := load(r.Context(), tx, providerID, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(current.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, p, map[string]string{"provider_id": providerID, "slot_id": slotID})
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	rows, err := loadSlots(r.Context(), tx, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	var slot *slotRow
	for i := range rows {
		if rows[i].ID == slotID {
			slot = &rows[i]
			break
		}
	}
	if slot == nil {
		return access.Reply{}, access.Fail(404, "slot_not_found", "The credential slot is unavailable.")
	}
	if err = matchSlot(r, current, slot); err != nil {
		return access.Reply{}, err
	}
	if slot.Default {
		return access.Reply{}, access.Fail(409, "default_slot_required", "The default credential slot cannot be deleted.")
	}
	// Immutable credential versions and published revision slots survive draft
	// removal. Only pending enrollment state belongs exclusively to this slot.
	if _, err = tx.Exec(r.Context(), `DELETE FROM olp.secrets WHERE purpose=$3 AND id IN (SELECT id FROM olp.grant_enrollments WHERE slot_id=$1 AND provider_id=$2)`, slotID, providerID, secrets.GrantEnrollment); err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.provider_slots WHERE id=$1 AND provider_id=$2", slotID, providerID); err != nil {
		return access.Reply{}, err
	}
	etag, err := touch(r.Context(), tx, providerID)
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.providers SET slots_etag=$2 WHERE id=$1", providerID, access.NewID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.slot.delete", "provider_slot", slotID, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusNoContent, ParentMutation: &access.ParentMutation{Previous: current.ETag, Current: etag}}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}
