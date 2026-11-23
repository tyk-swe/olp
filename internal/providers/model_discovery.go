package providers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
)

// The project scope is decided first, so a provider outside it answers 404
// before its precondition or credential is consulted.
func (s *Server) prepare(r *http.Request, p authz.Principal, id string) (*record, probe.Target, *slotRow, error) {
	tx, err := s.access.Pool.Begin(r.Context())
	if err != nil {
		return nil, probe.Target{}, nil, err
	}
	defer tx.Rollback(r.Context())
	current, err := readProbeProvider(r, p, tx, id)
	if err != nil {
		return nil, probe.Target{}, nil, err
	}
	credential, slot, err := s.credentialSlotFor(r.Context(), tx, current)
	if err != nil {
		return nil, probe.Target{}, nil, err
	}
	rows, err := tx.Query(r.Context(), "SELECT upstream_model FROM olp.provider_models WHERE provider_id=$1 ORDER BY enabled DESC,upstream_model LIMIT 2000", id)
	if err != nil {
		return nil, probe.Target{}, nil, err
	}
	models, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, probe.Target{}, nil, err
	}
	return current, current.target(credential, models...), slot, nil
}

func readProbeProvider(r *http.Request, p authz.Principal, q database.Queryer, id string) (*record, error) {
	current, err := readProvider(r.Context(), q, id)
	if err != nil {
		return nil, err
	}
	if err := p.Project(current.ProjectID, authz.Change); err != nil {
		return nil, err
	}
	return current, httpkit.Match(r, current.ETag)
}

func (s *Server) probe(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	current, target, _, err := s.prepare(r, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	at := time.Now().UTC()
	models, err := s.prober.ListModelFacts(r.Context(), target)
	var discovered any
	succeeded := err == nil
	var detail string
	if succeeded {
		discovered = len(models)
		detail = fmt.Sprintf("Reached the upstream; %d models listed.", len(models))
	} else {
		detail = probe.Classify(err).Detail
	}
	if err = s.recordProbe(r, "provider.probe", id, at, succeeded, detail); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"provider_id": id, "succeeded": succeeded, "checked_at": at, "probe_type": "models", "detail": detail, "discovered_models": discovered}, current.ETag), nil
}

// The upstream call ran without the installation lock, so authority and
// project reach are decided again under it before anything is written.
func (s *Server) recordProbe(r *http.Request, action, id string, at time.Time, succeeded bool, detail string) error {
	_, err := access.Transact(s.access, r, changeable(r.Context(), id), func(tx pgx.Tx, p authz.Principal, _ *record) (httpkit.Reply, access.Event, error) {
		if err := storeProbe(r.Context(), tx, id, at, succeeded, detail); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Reply{}, access.Event{Action: action, Resource: "provider", ID: id, Failed: !succeeded}, nil
	})
	return err
}

func probeStatus(succeeded bool) string {
	if succeeded {
		return "succeeded"
	}
	return "failed"
}

type discoverRequest struct {
	Models []struct {
		UpstreamModel string `json:"upstream_model"`
		DisplayName   string `json:"display_name"`
	} `json:"models"`
}

func (s *Server) discover(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input discoverRequest
	if err = httpkit.DecodeUnique(r, &input, 1<<20); err != nil {
		return httpkit.Reply{}, err
	}
	if len(input.Models) > 2000 {
		return httpkit.Reply{}, httpkit.Invalid("models", "Declare at most 2000 models per request.")
	}
	var models []probe.DiscoveredModel
	seen := map[string]bool{}
	for _, m := range input.Models {
		if err = connection.ValidModelName("models.upstream_model", m.UpstreamModel); err != nil {
			return httpkit.Reply{}, err
		}
		if m.DisplayName == "" {
			m.DisplayName = m.UpstreamModel
		}
		if err = connection.ValidModelName("models.display_name", m.DisplayName); err != nil {
			return httpkit.Reply{}, err
		}
		if !seen[m.UpstreamModel] {
			seen[m.UpstreamModel] = true
			models = append(models, probe.DiscoveredModel{Name: m.UpstreamModel, Display: m.DisplayName})
		}
	}
	current, target, slot, err := s.prepare(r, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	upstream := len(models) == 0
	at := time.Now().UTC()
	var listing *routing.RevisionListing
	if upstream {
		listed, provenance, err := s.listUpstreamModels(r.Context(), current.Connection.Document(), target, slot, at)
		if err != nil {
			pe := probe.Classify(err)
			if err := s.recordProbe(r, "provider.discover", id, at, false, pe.Detail); err != nil {
				return httpkit.Reply{}, err
			}
			return httpkit.Reply{}, httpkit.Fail(422, "discovery_failed", pe.Detail)
		}
		models, listing = append(models, listed...), provenance
	}
	return access.Transact(s.access, r, unchangedSince(r.Context(), current, "discovery"), func(tx pgx.Tx, p authz.Principal, locked *record) (httpkit.Reply, access.Event, error) {
		if err := s.storeDiscoveredModels(r.Context(), tx, locked, models, upstream, at, listing); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.detailReply(r.Context(), tx, id)
		return reply, access.Event{Action: "provider.discover", Resource: "provider", ID: id}, err
	})
}

func (s *Server) listUpstreamModels(ctx context.Context, cfg connection.Configuration, target probe.Target, slot *slotRow, at time.Time) ([]probe.DiscoveredModel, *routing.RevisionListing, error) {
	if cfg.Kind != connectors.KindAnthropic {
		listed, err := s.prober.ListModelFacts(ctx, target)
		return listed, nil, err
	}
	listing, err := s.prober.ListCompleteModelFacts(ctx, target)
	if err != nil || !listing.Complete {
		return listing.Models, nil, err
	}
	return listing.Models, completeListingProvenance(cfg, slot, at), nil
}
