package pricebook

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/param"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (s *API) listRateCards(r *http.Request, _ access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		cardColumns+" WHERE c.id<$1::uuid ORDER BY c.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []RateCard{}
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, card)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	cardIDs := make([]string, len(items))
	for i := range items {
		cardIDs[i] = items[i].ID
	}
	summaries, err := loadCardRevisionSummariesByCards(r.Context(), s.Access.Pool, cardIDs)
	if err != nil {
		return access.Reply{}, err
	}
	for i := range items {
		items[i].Revisions = summaries[items[i].ID]
		if items[i].Revisions == nil {
			items[i].Revisions = []CardRevision{}
		}
	}
	return access.ListReplyBy(items, page, func(c RateCard) string { return c.ID }), nil
}

func (s *API) createRateCard(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input rateCardInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	owner, err := parseOwner(input.Owner.Kind, input.Owner.ID)
	if err != nil {
		return access.Reply{}, err
	}
	if err = ownerExists(r.Context(), tx, owner); err != nil {
		return access.Reply{}, err
	}
	id, err := CreateRateCard(r.Context(), tx, principal.UserID(), owner)
	if err != nil {
		return access.Reply{}, err
	}
	card, err := scanCard(tx.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
	if err != nil {
		return access.Reply{}, fmt.Errorf("store rate card: %w", err)
	}
	result := access.Reply{Status: http.StatusCreated,
		Location: "/api/v1/rate-cards/" + card.ID, Body: card}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "rate_card.create",
		"rate_card", card.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}

func (s *API) getRateCard(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	card, err := scanCard(s.Access.Pool.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
	if err != nil {
		return access.Reply{}, err
	}
	revisions, err := loadCardRevisionSummaries(r.Context(), s.Access.Pool, card.ID)
	if err != nil {
		return access.Reply{}, err
	}
	card.Revisions = revisions
	return access.OK(card), nil
}

func (s *API) listRateCardRevisions(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	limit, err := param.Limit(r.URL.Query())
	if err != nil {
		return access.Reply{}, err
	}
	before := 1 << 30
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		revision, parseErr := strconv.Atoi(raw)
		if parseErr != nil || revision < 1 {
			return access.Reply{}, access.Fail(400, "invalid_cursor", "The cursor is invalid.")
		}
		before = revision
	}
	rows, err := s.Access.Pool.Query(r.Context(), cardRevisionColumns+
		" WHERE r.rate_card_id=$1::uuid AND r.revision < $2 ORDER BY r.revision DESC LIMIT $3",
		id, before, int64(limit)+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []CardRevision{}
	for rows.Next() {
		revision, err := scanCardRevision(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, revision)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return access.Reply{}, err
	}
	var next *string
	if len(items) > limit {
		items = items[:limit]
		cursor := fmt.Sprint(items[len(items)-1].Revision)
		next = &cursor
	}
	revisionIDs := make([]string, len(items))
	for i := range items {
		revisionIDs[i] = items[i].ID
	}
	pricesByRevision, err := LoadCardPricesByRevisions(r.Context(), s.Access.Pool, revisionIDs)
	if err != nil {
		return access.Reply{}, err
	}
	for i := range items {
		items[i].Prices = pricesByRevision[items[i].ID]
		if items[i].Prices == nil {
			items[i].Prices = []rates.Price{}
		}
	}
	return access.OK(param.Page{Items: items, NextCursor: next}), nil
}

func (s *API) createRateCardRevision(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input cardRevisionInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	cardID, err := access.IDParam(r, "rate_card_id")
	if err != nil {
		return access.Reply{}, err
	}
	if input.Markup == nil {
		return access.Reply{}, access.Invalid("markup", "A rate card revision needs a markup.")
	}
	tx, principal, err := s.Access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	var exists bool
	if err = tx.QueryRow(r.Context(),
		"SELECT EXISTS(SELECT 1 FROM olp.rate_cards WHERE id=$1::uuid)", cardID).Scan(&exists); err != nil {
		return access.Reply{}, err
	}
	if !exists {
		return access.Reply{}, access.Fail(http.StatusNotFound, "rate_card_not_found", "No such rate card.")
	}
	effectiveAt := time.Now().UTC()
	if input.EffectiveAt != nil {
		effectiveAt = input.EffectiveAt.UTC()
		if effectiveAt.Before(time.Now()) {
			return access.Reply{}, access.Invalid("effective_at",
				"Choose an effective time now or in the future.")
		}
	}
	currency, err := InstallationCurrency(r.Context(), tx)
	if err != nil {
		return access.Reply{}, err
	}
	revision, err := CreateCardRevision(r.Context(), tx, principal.UserID(), cardID, *input.Markup, effectiveAt, input.Prices, currency, s.Normalizer)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusCreated,
		Location: "/api/v1/rate-cards/" + cardID + "/revisions/" + fmt.Sprint(revision.Revision),
		Body:     revision}
	if err = access.Audit(r.Context(), tx, r, principal.Actor(), "rate_card.publish",
		"rate_card", cardID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
