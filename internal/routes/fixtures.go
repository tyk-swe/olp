package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	maxFixtureFileBytes = 256 << 10
	maxFixtures         = 16
)

var fixtureName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_. -]{0,63}$`)

type fixtureFile struct {
	Synthetic bool           `json:"synthetic"`
	Fixtures  []routeFixture `json:"fixtures"`
}

type routeFixture struct {
	Name             string            `json:"name"`
	Operation        string            `json:"operation"`
	Surface          string            `json:"surface"`
	Mode             string            `json:"mode"`
	Dialect          string            `json:"dialect"`
	Seed             string            `json:"seed"`
	Request          json.RawMessage   `json:"request"`
	ClassifierLabels map[string]string `json:"classifier_labels"`
	ExpectEligible   *bool             `json:"expect_eligible"`
}

func (f routeFixture) input() simulationInput {
	return simulationInput{
		Operation: f.Operation, Surface: f.Surface, Mode: f.Mode, Dialect: f.Dialect,
		Seed: f.Seed, Request: f.Request, ClassifierLabels: f.ClassifierLabels,
	}
}

func (f fixtureFile) validate() error {
	if !f.Synthetic {
		return httpkit.Invalid("synthetic", "Confirm the file uses only synthetic requests and labels.")
	}
	if len(f.Fixtures) < 1 || len(f.Fixtures) > maxFixtures {
		return httpkit.Invalid("fixtures", "Use between 1 and 16 fixtures.")
	}
	seen := make(map[string]bool, len(f.Fixtures))
	for i, item := range f.Fixtures {
		field := fmt.Sprintf("fixtures.%d", i)
		if !fixtureName.MatchString(item.Name) || seen[item.Name] {
			return httpkit.Invalid(field+".name", "Use a distinct name of 1 to 64 letters, digits, spaces, dots, underscores or hyphens, starting with a letter or digit.")
		}
		seen[item.Name] = true
		if len(item.Seed) == 0 || len(item.Seed) > 256 {
			return httpkit.Invalid(field+".seed", "Use a seed of 1 to 256 bytes.")
		}
		if err := validTuple(item.Operation, item.Surface, item.Mode); err != nil {
			return httpkit.Invalid(field, "Choose a supported operation, surface and mode.")
		}
		if !bytes.HasPrefix(bytes.TrimSpace(item.Request), []byte("{")) {
			return httpkit.Invalid(field+".request", "Provide a synthetic native request object.")
		}
		if err := validSimulationLabels(item.input()); err != nil {
			return httpkit.Invalid(field+".classifier_labels", "Use at most 16 labels with names of 1 to 64 bytes and values of at most 256 bytes.")
		}
	}
	return nil
}

type fixtureReport struct {
	PublishedRevisionID string          `json:"published_revision_id"`
	PublishedRevision   int             `json:"published_revision"`
	DraftETag           string          `json:"draft_etag"`
	PlanningTime        time.Time       `json:"planning_time"`
	LiveSupply          string          `json:"live_supply"`
	Passed              bool            `json:"passed"`
	Results             []fixtureResult `json:"results"`
}

func (s *Server) checkFixtures(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "draft_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var file fixtureFile
	if err := httpkit.DecodeUnique(r, &file, maxFixtureFileBytes); err != nil {
		return httpkit.Reply{}, err
	}
	if err := file.validate(); err != nil {
		return httpkit.Reply{}, err
	}
	ctx := r.Context()
	tx, err := s.Access.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(ctx)
	d, err := loadDraft(ctx, tx, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	snapshot, err := release.ReadSnapshot(ctx, tx)
	if err != nil {
		return httpkit.Reply{}, err
	}
	published, ok := snapshot.Routes[d.Slug]
	if !ok || !sameProject(d.ProjectID, published.ProjectID) {
		return httpkit.Reply{}, httpkit.Fail(409, "no_published_fixture_baseline", "Publish a revision for this route in the draft's project before checking fixtures.")
	}
	if err := ValidateFidelityPolicy(d.Fidelity, d.ContentPolicy); err != nil {
		return httpkit.Reply{}, err
	}
	draftRoute, err := draftSimulationRoute(ctx, tx, d, published.ID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	withDraft := *snapshot
	withDraft.Routes = maps.Clone(snapshot.Routes)
	withDraft.Routes[d.Slug] = draftRoute
	shared, err := s.fixtureSimulation(r, tx, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	results, err := s.compareFixtures(shared, snapshot, &withDraft, d.Slug, file.Fixtures)
	if err != nil {
		return httpkit.Reply{}, err
	}
	passed := true
	for _, result := range results {
		passed = passed && result.Passed
	}
	return httpkit.OK(fixtureReport{
		PublishedRevisionID: published.RevisionID, PublishedRevision: published.Revision, DraftETag: d.ETag,
		PlanningTime: shared.now, LiveSupply: "not_simulated", Passed: passed, Results: results,
	}), nil
}

func (s *Server) fixtureSimulation(r *http.Request, tx pgx.Tx, p authz.Principal) (simulation, error) {
	inputs, err := s.routingInputs(r, tx)
	if err != nil {
		return simulation{}, err
	}
	ineligible, err := release.ReadIneligible(r.Context(), tx, nil)
	if err != nil {
		return simulation{}, err
	}
	return simulation{
		ctx: r.Context(), inputs: inputs, now: time.Now().UTC(),
		key: func(route routing.Route) (inspectionKeyContext, error) {
			return s.inspectionKey(r, tx, p, nil, route)
		},
		eligibility: func(routing.Route) (func(string) routing.Eligibility, error) {
			return func(id string) routing.Eligibility { return ineligible[id] }, nil
		},
	}, nil
}

func (s *Server) compareFixtures(shared simulation, published, draft *routing.Snapshot, slug string, fixtures []routeFixture) ([]fixtureResult, error) {
	results := make([]fixtureResult, 0, len(fixtures))
	for i, fixture := range fixtures {
		baseline, err := s.fixturePlan(shared, published, fixture.input(), slug)
		if err != nil {
			return nil, fixturePlanError(i, err)
		}
		proposed, err := s.fixturePlan(shared, draft, fixture.input(), slug)
		if err != nil {
			return nil, fixturePlanError(i, err)
		}
		results = append(results, fixtureResult{
			Name: fixture.Name, Published: baseline, Draft: proposed,
			Changes: diffFixturePlans(baseline, proposed), ExpectEligible: fixture.ExpectEligible,
			Passed: fixture.ExpectEligible == nil || *fixture.ExpectEligible == (proposed.Winner != nil),
		})
	}
	return results, nil
}

func fixturePlanError(index int, err error) error {
	var problem *httpkit.Problem
	if errors.As(err, &problem) && problem.Status == http.StatusUnprocessableEntity {
		return httpkit.Invalid(fmt.Sprintf("fixtures.%d", index), "The request could not be inspected for both revisions. Check its route, dialect and delivery mode.")
	}
	return err
}
