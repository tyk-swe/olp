package usage

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// Server exposes the read side of accounting — usage reports, request history,
// pricing revisions, and the gateway epochs that bound what was lost — over the
// management API. Every route authenticates as a console session through the
// access server; nothing here is reachable with an API key.
type Server struct {
	Access *access.Server
	// VendorKind resolves a vendor catalogue identifier to its connector kind.
	// Pricing takes it as a function so accounting never depends on the
	// provider package.
	VendorKind func(vendor string) (string, bool)

	Egress *egress.Policy
}

// Register mounts the accounting routes. The patterns are more specific than
// the management catch-all, so they take precedence over its 404.
func (s *Server) Register(mux *http.ServeMux) {
	h := s.Access.Handle
	mux.HandleFunc("GET /api/v3/usage/summary", h(s.usageSummary))
	mux.HandleFunc("GET /api/v3/usage/breakdown", h(s.usageBreakdown))
	mux.HandleFunc("GET /api/v3/usage/time-series", h(s.usageTimeSeries))
	mux.HandleFunc("GET /api/v3/usage/completeness", h(s.usageCompleteness))
	mux.HandleFunc("GET /api/v3/requests", h(s.listRequests))
	mux.HandleFunc("GET /api/v3/requests/{request_id}", h(s.getRequest))
	mux.HandleFunc("GET /api/v3/pricing/revisions", h(s.listPricingRevisions))
	// A revision may carry thousands of rates, well past the default body cap.
	mux.HandleFunc("POST /api/v3/pricing/revisions", s.Access.HandleWith(1<<20, s.createPricingRevision))
	mux.HandleFunc("GET /api/v3/pricing/sources", h(s.listPricingSources))
	mux.HandleFunc("POST /api/v3/pricing/sources", h(s.createPricingSource))
	mux.HandleFunc("GET /api/v3/pricing/sources/{pricing_source_id}", h(s.getPricingSource))
	mux.HandleFunc("PATCH /api/v3/pricing/sources/{pricing_source_id}", h(s.updatePricingSource))
	mux.HandleFunc("POST /api/v3/pricing/sources/{pricing_source_id}/refresh", h(s.refreshPricingSource))
	mux.HandleFunc("GET /api/v3/pricing/sources/{pricing_source_id}/snapshots", h(s.listPricingSourceSnapshots))

	mux.HandleFunc("POST /api/v3/pricing/source-snapshots/{pricing_source_snapshot_id}/publish",
		s.Access.HandleWith(1<<20, s.publishPricingSourceSnapshot))
	mux.HandleFunc("GET /api/v3/request-metadata/gateway-epochs", h(s.listGatewayEpochs))
	mux.HandleFunc("POST /api/v3/request-metadata/gateway-epochs/{process_epoch}/acknowledge",
		h(s.acknowledgeGatewayEpoch))
}

// list is the shape every paged accounting response shares.
type list struct {
	Items      any     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// read authorises a console read. Operations reads are the lowest management
// permission: every active role may see what the installation spent.
func (s *Server) read(r *http.Request) (access.Principal, error) {
	return s.Access.Principal(r, s.Access.Pool, "usage")
}

func (s *Server) usageSummary(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters, err := usageFilters(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters.AllProjects, filters.AllowedProjects = p.AllProjects, p.ProjectIDs()
	summary, err := ReadSummary(r.Context(), s.Access.Pool, filters, time.Now())
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(summary), nil
}

func (s *Server) usageCompleteness(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters, err := usageFilters(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters.AllProjects, filters.AllowedProjects = p.AllProjects, p.ProjectIDs()
	report, err := ReadCompleteness(r.Context(), s.Access.Pool, filters, time.Now())
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}

func (s *Server) usageBreakdown(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters, err := usageFilters(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters.AllProjects, filters.AllowedProjects = p.AllProjects, p.ProjectIDs()
	limit, err := limitParam(r.URL.Query())
	if err != nil {
		return access.Reply{}, err
	}
	report, err := ReadBreakdown(r.Context(), s.Access.Pool, filters,
		r.URL.Query().Get("dimension"), limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}

func (s *Server) usageTimeSeries(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters, err := usageFilters(r)
	if err != nil {
		return access.Reply{}, err
	}
	filters.AllProjects, filters.AllowedProjects = p.AllProjects, p.ProjectIDs()
	granularity := GranularityHour
	if raw := strings.TrimSpace(r.URL.Query().Get("granularity")); raw != "" {
		granularity = raw
	}
	series, err := ReadSeries(r.Context(), s.Access.Pool, filters, granularity)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(series), nil
}

func (s *Server) listRequests(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	filters := RequestFilters{
		Route:           textParam(query, "route"),
		Model:           textParam(query, "model"),
		Operation:       textParam(query, "operation"),
		ErrorClass:      textParam(query, "error_class"),
		AllProjects:     p.AllProjects,
		AllowedProjects: p.ProjectIDs(),
	}
	if filters.ProviderID, err = uuidParam(query, "provider_id"); err != nil {
		return access.Reply{}, err
	}
	if filters.APIKey, err = uuidParam(query, "api_key_id"); err != nil {
		return access.Reply{}, err
	}
	if filters.StatusCode, err = statusParam(query); err != nil {
		return access.Reply{}, err
	}
	if filters.StartedAfter, err = timeParam(query, "started_after"); err != nil {
		return access.Reply{}, err
	}
	if filters.StartedBefore, err = timeParam(query, "started_before"); err != nil {
		return access.Reply{}, err
	}
	if err = filters.Validate(); err != nil {
		return access.Reply{}, err
	}
	cursor, err := cursorParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	limit, err := limitParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	items, next, err := ListRequests(r.Context(), s.Access.Pool, filters, cursor, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(list{Items: items, NextCursor: next}), nil
}

func (s *Server) getRequest(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "request_id")
	if err != nil {
		return access.Reply{}, err
	}
	detail, err := GetRequest(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	var keyProject *string
	if err = s.Access.Pool.QueryRow(r.Context(), "SELECT project_id::text FROM olp_go.api_keys WHERE id=$1", detail.APIKeyID).Scan(&keyProject); err != nil {
		return access.Reply{}, err
	}
	if !p.CanProject(keyProject, false) {
		return access.Reply{}, access.Fail(404, "not_found", "The request does not exist.")
	}
	return access.OK(detail), nil
}

func (s *Server) listPricingRevisions(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	if !p.AllProjects {
		return access.Reply{}, access.Forbidden()
	}
	query := r.URL.Query()
	limit, err := limitParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	var before *int
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		revision, parseErr := strconv.Atoi(raw)
		if parseErr != nil || revision < 1 {
			return access.Reply{}, access.Fail(400, "invalid_cursor", "The cursor is invalid.")
		}
		before = &revision
	}
	items, next, err := ListRevisions(r.Context(), s.Access.Pool, before, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(list{Items: items, NextCursor: next}), nil
}

// pricingRevisionInput is the documented creation payload. effective_at is a
// pointer so an omitted field is refused instead of defaulting to the zero
// time, which would silently backdate every rate.
type pricingRevisionInput struct {
	EffectiveAt *time.Time `json:"effective_at"`
	Prices      []Price    `json:"prices"`
}

func (s *Server) createPricingRevision(r *http.Request) (access.Reply, error) {
	var input pricingRevisionInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.EffectiveAt == nil {
		return access.Reply{}, access.Invalid("effective_at",
			"Give the time these prices take effect.")
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Principal(r, tx, "settings")
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := s.Access.Replay(r, tx, principal, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	revision, err := CreateRevision(r.Context(), tx, principal.UserID(), *input.EffectiveAt,
		input.Prices, s.VendorKind)
	if err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, Body: revision}
	if err = access.Audit(r.Context(), tx, r, principal.ID, "pricing_revision.create",
		"pricing_revision", revision.ID, "success"); err != nil {
		return access.Reply{}, err
	}
	if err = s.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (s *Server) listGatewayEpochs(r *http.Request) (access.Reply, error) {
	p, err := s.read(r)
	if err != nil {
		return access.Reply{}, err
	}
	if !p.AllProjects {
		return access.Reply{}, access.Forbidden()
	}
	query := r.URL.Query()
	limit, err := limitParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	cursor, err := cursorParam(query)
	if err != nil {
		return access.Reply{}, err
	}
	items, next, err := ListGatewayEpochs(r.Context(), s.Access.Pool,
		textParam(query, "state"), cursor, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(list{Items: items, NextCursor: next}), nil
}

func (s *Server) acknowledgeGatewayEpoch(r *http.Request) (access.Reply, error) {
	epoch, err := access.IDParam(r, "process_epoch")
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(context.WithoutCancel(r.Context()))
	principal, err := s.Access.Principal(r, tx, "settings")
	if err != nil {
		return access.Reply{}, err
	}
	acknowledgement, err := AcknowledgeGatewayEpoch(r.Context(), tx, epoch, principal.UserID())
	if err != nil {
		return access.Reply{}, err
	}
	if acknowledgement == nil {
		return access.Reply{}, access.Fail(404, "not_found",
			"No unresolved gateway epoch with that identifier was found.")
	}
	// Acknowledging is an operator statement about loss they have seen, so each
	// acknowledgement is audited even when the epoch was already acknowledged.
	if err = access.Audit(r.Context(), tx, r, principal.ID,
		"request_metadata.gateway_epoch_acknowledge", "request_metadata_gateway_epoch",
		epoch, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.OK(acknowledgement))
}

// usageFilters reads the shared usage query. Times are required and the range
// is validated here so every report refuses the same impossible windows.
func usageFilters(r *http.Request) (Filters, error) {
	query := r.URL.Query()
	start, err := requiredTimeParam(query, "start")
	if err != nil {
		return Filters{}, err
	}
	end, err := requiredTimeParam(query, "end")
	if err != nil {
		return Filters{}, err
	}
	filters := Filters{
		Start:            start,
		End:              end,
		Route:            textParam(query, "route"),
		Model:            textParam(query, "model"),
		Operation:        textParam(query, "operation"),
		AttributionKey:   textParam(query, "attribution_key"),
		AttributionValue: textParam(query, "attribution_value"),
	}
	if filters.ProviderID, err = uuidParam(query, "provider_id"); err != nil {
		return Filters{}, err
	}
	if filters.APIKey, err = uuidParam(query, "api_key_id"); err != nil {
		return Filters{}, err
	}
	if err = filters.Validate(); err != nil {
		return Filters{}, err
	}
	return filters, nil
}

// textParam treats a blank parameter as absent: a console that clears a
// filter sends an empty value, and filtering on the empty string would return
// an empty page that looks like "no traffic".
func textParam(query url.Values, name string) *string {
	value := strings.TrimSpace(query.Get(name))
	if value == "" {
		return nil
	}
	return &value
}

func uuidParam(query url.Values, name string) (*string, error) {
	raw := textParam(query, name)
	if raw == nil {
		return nil, nil
	}
	id, err := access.ParseUUID(*raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_filter", "Use a valid identifier for "+name+".")
	}
	return &id, nil
}

func requiredTimeParam(query url.Values, name string) (time.Time, error) {
	value, err := timeParam(query, name)
	if err != nil {
		return time.Time{}, err
	}
	if value == nil {
		return time.Time{}, access.Fail(400, "invalid_range",
			"Give an RFC 3339 "+name+" for the reporting range.")
	}
	return *value, nil
}

func timeParam(query url.Values, name string) (*time.Time, error) {
	raw := textParam(query, name)
	if raw == nil {
		return nil, nil
	}
	value, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_range", "Use an RFC 3339 timestamp for "+name+".")
	}
	utc := value.UTC()
	return &utc, nil
}

// statusParam bounds the status filter to the codes a request can actually
// carry, so a typo returns an error instead of an empty page.
func statusParam(query url.Values) (*int, error) {
	raw := textParam(query, "status_code")
	if raw == nil {
		return nil, nil
	}
	status, err := strconv.Atoi(*raw)
	if err != nil || status < 100 || status > 599 {
		return nil, access.Fail(400, "invalid_filter", "Use an HTTP status code from 100 to 599.")
	}
	return &status, nil
}

func limitParam(query url.Values) (int, error) {
	raw := textParam(query, "limit")
	if raw == nil {
		return 50, nil
	}
	limit, err := strconv.Atoi(*raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, access.Fail(400, "invalid_limit", "Use a page size from 1 to 200.")
	}
	return limit, nil
}

func cursorParam(query url.Values) (*Cursor, error) {
	raw := textParam(query, "cursor")
	if raw == nil {
		return nil, nil
	}
	at, id, err := DecodeCursor(*raw)
	if err != nil {
		return nil, access.Fail(400, "invalid_cursor", "The cursor is invalid.")
	}
	return &Cursor{At: at, ID: id}, nil
}
