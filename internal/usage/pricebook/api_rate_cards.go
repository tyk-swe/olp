package pricebook

import (
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/usage/param"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func (s *API) listRateCards(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(),
		cardColumns+" WHERE c.id<$1::uuid ORDER BY c.id DESC LIMIT $2", page.Before, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []RateCard{}
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, card)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
	}
	cardIDs := make([]string, len(items))
	for i := range items {
		cardIDs[i] = items[i].ID
	}
	summaries, err := loadCardRevisionSummariesByCards(r.Context(), s.Access.Pool, cardIDs)
	if err != nil {
		return httpkit.Reply{}, err
	}
	for i := range items {
		items[i].Revisions = summaries[items[i].ID]
		if items[i].Revisions == nil {
			items[i].Revisions = []CardRevision{}
		}
	}
	return httpkit.ListReplyBy(items, page, func(c RateCard) string { return c.ID }), nil
}

func (s *API) createRateCard(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input rateCardInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(s.Access, r, input, access.Unlocked, func(tx pgx.Tx, principal authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		owner, err := parseOwner(input.Owner.Kind, input.Owner.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = ownerExists(r.Context(), tx, owner); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, err := CreateRateCard(r.Context(), tx, principal.UserID(), owner)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		card, err := scanCard(tx.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
		if err != nil {
			return httpkit.Reply{}, access.Event{}, fmt.Errorf("store rate card: %w", err)
		}
		result := httpkit.Reply{Status: http.StatusCreated,
			Location: "/api/v1/rate-cards/" + card.ID, Body: card}
		return result, access.Event{Action: "rate_card.create", Resource: "rate_card", ID: card.ID}, nil
	})
}

func (s *API) getRateCard(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "rate_card_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	card, err := scanCard(s.Access.Pool.QueryRow(r.Context(), cardColumns+" WHERE c.id=$1::uuid", id))
	if err != nil {
		return httpkit.Reply{}, err
	}
	revisions, err := loadCardRevisionSummaries(r.Context(), s.Access.Pool, card.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	card.Revisions = revisions
	return httpkit.OK(card), nil
}

func (s *API) listRateCardRevisions(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "rate_card_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	query := r.URL.Query()
	limit, err := param.Limit(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	before, err := param.RevisionCursor(query)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), cardRevisionColumns+
		" WHERE r.rate_card_id=$1::uuid AND ($2::int IS NULL OR r.revision < $2) ORDER BY r.revision DESC LIMIT $3",
		id, before, int64(limit)+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []CardRevision{}
	for rows.Next() {
		revision, err := scanCardRevision(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, revision)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return httpkit.Reply{}, err
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
		return httpkit.Reply{}, err
	}
	for i := range items {
		items[i].Prices = pricesByRevision[items[i].ID]
		if items[i].Prices == nil {
			items[i].Prices = []rates.Price{}
		}
	}
	return httpkit.OK(param.Page{Items: items, NextCursor: next}), nil
}

func (s *API) createRateCardRevision(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input cardRevisionInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	cardID, err := httpkit.IDParam(r, "rate_card_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if input.Markup == nil {
		return httpkit.Reply{}, httpkit.Invalid("markup", "A rate card revision needs a markup.")
	}
	return access.Mutate(s.Access, r, input, access.Unlocked, func(tx pgx.Tx, principal authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var exists bool
		if err = tx.QueryRow(r.Context(),
			"SELECT EXISTS(SELECT 1 FROM olp.rate_cards WHERE id=$1::uuid)", cardID).Scan(&exists); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !exists {
			return httpkit.Reply{}, access.Event{}, httpkit.Fail(http.StatusNotFound, "rate_card_not_found", "No such rate card.")
		}
		effectiveAt := time.Now().UTC()
		if input.EffectiveAt != nil {
			effectiveAt = input.EffectiveAt.UTC()
			if effectiveAt.Before(time.Now()) {
				return httpkit.Reply{}, access.Event{}, httpkit.Invalid("effective_at",
					"Choose an effective time now or in the future.")
			}
		}
		currency, err := InstallationCurrency(r.Context(), tx)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		revision, err := CreateCardRevision(r.Context(), tx, principal.UserID(), cardID, *input.Markup, effectiveAt, input.Prices, currency, s.Normalizer)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		result := httpkit.Reply{Status: http.StatusCreated,
			Location: "/api/v1/rate-cards/" + cardID + "/revisions/" + fmt.Sprint(revision.Revision),
			Body:     revision}
		return result, access.Event{Action: "rate_card.publish", Resource: "rate_card", ID: cardID}, nil
	})
}
