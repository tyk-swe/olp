package reports

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage/param"
)

// Server exposes the read side of accounting — usage reports, request history,
// pricing revisions, and the gateway epochs that bound what was lost — over the
// management API. Every route admits the console session or management token
// its contract requirement names; nothing here is reachable with a gateway API
// key.
type API struct {
	Access *access.Server
}

func (s *API) Register(mux *http.ServeMux) {
	s.registerCodeMode(mux)
	s.route(mux, "GET /api/v1/usage/summary", s.usageSummary)
	s.route(mux, "GET /api/v1/usage/breakdown", s.usageBreakdown)
	s.route(mux, "GET /api/v1/usage/time-series", s.usageTimeSeries)
	s.route(mux, "GET /api/v1/usage/completeness", s.usageCompleteness)
	s.route(mux, "GET /api/v1/usage/selector-savings", s.selectorSavings)
	s.route(mux, "GET /api/v1/usage/shadow-experiments", s.shadowExperiments)
	s.stream(mux, "GET /api/v1/requests/export.csv", s.exportRequestsCSV)
	s.stream(mux, "GET /api/v1/usage/export.csv", s.exportUsageCSV)
	s.route(mux, "GET /api/v1/requests", s.listRequests)
	s.route(mux, "GET /api/v1/requests/{request_id}", s.getRequest)
}

func (s *API) route(mux *http.ServeMux, pattern string, handler access.Handler) {
	s.Access.Route(mux, pattern, func(r *http.Request, principal access.Principal) (access.Reply, error) {
		reply, err := handler(r, principal)
		return reply, problem(err)
	})
}

func (s *API) stream(mux *http.ServeMux, pattern string, handler access.StreamHandler) {
	s.Access.Stream(mux, pattern, func(w http.ResponseWriter, r *http.Request, principal access.Principal) error {
		return problem(handler(w, r, principal))
	})
}

func (s *API) usageSummary(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	summary, err := ReadSummary(r.Context(), s.Access.Pool, filters, time.Now())
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(summary), nil
}

func (s *API) usageCompleteness(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	report, err := ReadCompleteness(r.Context(), s.Access.Pool, filters, time.Now())
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}

func (s *API) usageBreakdown(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	limit, err := param.Limit(r.URL.Query())
	if err != nil {
		return access.Reply{}, err
	}
	dimension, err := parseDimension(r.URL.Query().Get("dimension"), filters)
	if err != nil {
		return access.Reply{}, err
	}
	report, err := ReadBreakdown(r.Context(), s.Access.Pool, filters, dimension, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(report), nil
}

func (s *API) usageTimeSeries(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := usageFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	raw := strings.TrimSpace(r.URL.Query().Get("granularity"))
	if raw == "" {
		raw = string(GranularityHour)
	}
	granularity, err := parseGranularity(raw)
	if err != nil {
		return access.Reply{}, err
	}
	series, err := ReadSeries(r.Context(), s.Access.Pool, filters, granularity)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(series), nil
}

func (s *API) requestFilters(r *http.Request, p access.Principal) (RequestFilters, error) {
	var err error
	query := r.URL.Query()
	filters := RequestFilters{
		Route:            param.Text(query, "route"),
		Model:            param.Text(query, "model"),
		Operation:        param.Text(query, "operation"),
		ErrorClass:       param.Text(query, "error_class"),
		AttributionKey:   param.Text(query, "attribution_key"),
		AttributionValue: param.Text(query, "attribution_value"),
		Projects:         visibleProjects(p),
	}
	if filters.ProviderID, err = param.UUID(query, "provider_id"); err != nil {
		return filters, err
	}
	if filters.APIKey, err = param.UUID(query, "api_key_id"); err != nil {
		return filters, err
	}
	if filters.StatusCode, err = param.Status(query); err != nil {
		return filters, err
	}
	if filters.StartedAfter, err = param.Time(query, "started_after"); err != nil {
		return filters, err
	}
	if filters.StartedBefore, err = param.Time(query, "started_before"); err != nil {
		return filters, err
	}
	if filters.ProjectID, err = param.UUID(query, "project_id"); err != nil {
		return filters, err
	}
	if filters.ProjectID != nil {
		if err = p.Project(filters.ProjectID, access.View); err != nil {
			return filters, err
		}
	}
	if filters.AttributionKey, filters.AttributionValue, err = sessionAttribution(query, filters.AttributionKey, filters.AttributionValue); err != nil {
		return filters, err
	}
	return filters, filters.validate()
}

func (s *API) listRequests(r *http.Request, p access.Principal) (access.Reply, error) {
	filters, err := s.requestFilters(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	query := r.URL.Query()
	cursor, err := param.Cursor(query)
	if err != nil {
		return access.Reply{}, err
	}
	limit, err := param.Limit(query)
	if err != nil {
		return access.Reply{}, err
	}
	items, next, err := ListRequests(r.Context(), s.Access.Pool, filters, cursor, limit)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(param.Page{Items: items, NextCursor: next}), nil
}

func (s *API) getRequest(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "request_id")
	if err != nil {
		return access.Reply{}, err
	}
	detail, err := GetRequest(r.Context(), s.Access.Pool, id)
	if err != nil {
		return access.Reply{}, err
	}
	// A keyless request is the installation's own and belongs to no project.
	var keyProject *string
	if detail.APIKeyID != nil {
		if keyProject, err = readKeyProject(r.Context(), s.Access.Pool, *detail.APIKeyID); err != nil {
			return access.Reply{}, err
		}
	}
	if p.Project(keyProject, access.View) != nil {
		return access.Reply{}, access.Fail(404, "not_found", "The request does not exist.")
	}
	return access.OK(detail), nil
}

func visibleProjects(p access.Principal) ProjectScope {
	if p.AllProjects {
		return AllProjects()
	}
	return OnlyProjects(p.ProjectIDs()...)
}

// sessionAttribution folds session_id into the attribution filters, which is
// how a session is stored.
func sessionAttribution(query url.Values, key, value *string) (*string, *string, error) {
	session := param.Text(query, "session_id")
	if session == nil {
		return key, value, nil
	}
	if key != nil && *key != "session" || value != nil && *value != *session {
		return nil, nil, access.Fail(422, "invalid_filter", "session_id conflicts with the attribution filters.")
	}
	sessionKey := "session"
	return &sessionKey, session, nil
}

// usageFilters reads the shared usage query. Times are required and the range
// is validated here so every report refuses the same impossible windows.
func usageFilters(r *http.Request, p access.Principal) (Filters, error) {
	query := r.URL.Query()
	start, err := param.RequiredTime(query, "start")
	if err != nil {
		return Filters{}, err
	}
	end, err := param.RequiredTime(query, "end")
	if err != nil {
		return Filters{}, err
	}
	filters := Filters{
		Start:            start,
		End:              end,
		Route:            param.Text(query, "route"),
		Model:            param.Text(query, "model"),
		Operation:        param.Text(query, "operation"),
		AttributionKey:   param.Text(query, "attribution_key"),
		AttributionValue: param.Text(query, "attribution_value"),
	}
	if filters.ProviderID, err = param.UUID(query, "provider_id"); err != nil {
		return Filters{}, err
	}
	if filters.APIKey, err = param.UUID(query, "api_key_id"); err != nil {
		return Filters{}, err
	}
	filters.Projects = visibleProjects(p)
	if project, err := param.UUID(query, "project_id"); err != nil {
		return Filters{}, err
	} else if project != nil {
		if err := p.Project(project, access.View); err != nil {
			return Filters{}, err
		}
		filters.Projects = OnlyProjects(*project)
	}
	if filters.AttributionKey, filters.AttributionValue, err = sessionAttribution(query, filters.AttributionKey, filters.AttributionValue); err != nil {
		return Filters{}, err
	}
	if err = filters.validate(); err != nil {
		return Filters{}, err
	}
	return filters, nil
}
