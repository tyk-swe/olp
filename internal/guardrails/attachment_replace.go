package guardrails

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

func (s *Server) replaceAttachment(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "attachment_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input struct {
		PolicyRevisionID string `json:"policy_revision_id"`
	}
	if err := access.DecodeUnique(r, &input, 65536); err != nil {
		return access.Reply{}, err
	}
	revisionID, parseErr := access.ParseUUID(input.PolicyRevisionID)
	if parseErr != nil {
		return access.Reply{}, access.Invalid("policy_revision_id", "Choose a policy revision.")
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
	var current Attachment
	var etag string
	if err := tx.QueryRow(r.Context(), "SELECT id::text,scope,COALESCE(scope_id::text,'installation'),policy_revision_id::text,position,etag::text FROM olp.guardrail_attachments WHERE id=$1 FOR UPDATE", id).Scan(&current.ID, &current.Scope, &current.ScopeID, &current.PolicyRevisionID, &current.Order, &etag); err != nil {
		return access.Reply{}, err
	}
	if err := AuthorizeAttachment(r.Context(), tx, p, current, access.Change); err != nil {
		return access.Reply{}, err
	}
	claim, replay, err := s.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	next := Attachment{ID: current.ID, Scope: current.Scope, ScopeID: current.ScopeID, PolicyRevisionID: revisionID, Order: current.Order}
	if err := validateAttachmentPolicy(r.Context(), tx, p, next); err != nil {
		return access.Reply{}, err
	}
	nextETag := access.NewID()
	if _, err := tx.Exec(r.Context(), "UPDATE olp.guardrail_attachments SET policy_revision_id=$2,etag=$3 WHERE id=$1", id, next.PolicyRevisionID, nextETag); err != nil {
		return access.Reply{}, err
	}
	if err := s.publish(r.Context(), tx, p.UserID()); err != nil {
		return access.Reply{}, access.Invalid("attachment", "Activate the policy revision and check compatibility with attached routes.")
	}
	if err := access.Audit(r.Context(), tx, r, p.Actor(), "guardrail.attachment.replace", "guardrail_attachment", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Detail(struct {
		Attachment
		ETag string `json:"etag"`
	}{next, nextETag}, nextETag)
	if err := s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
