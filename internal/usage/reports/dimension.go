package reports

// Dimension is a grouping the breakdown report can answer.
type Dimension string

const (
	DimensionRoute              Dimension = "route"
	DimensionProvider           Dimension = "provider"
	DimensionModel              Dimension = "model"
	DimensionAPIKey             Dimension = "api_key"
	DimensionOperation          Dimension = "operation"
	DimensionAttribution        Dimension = "attribution"
	DimensionEndUser            Dimension = "end_user"
	DimensionProject            Dimension = "project"
	DimensionSession            Dimension = "session"
	DimensionModelFamily        Dimension = "model_family"
	DimensionEstimateProvenance Dimension = "estimate_provenance"
)

// Requests count once under the family of their first attempt, so the rows
// still sum to the range's requests, while tokens follow each attempt into its
// own family. Every attempt is recorded with the family of its model, estimated
// or not, so unknown is only the rows recorded before families were, and stays
// apart from the catch-all family "other".
const modelFamilyExpression = "COALESCE(NULLIF(model_family, ''), 'unknown')"

// dimensionExpressions leaves attribution empty because its key is a bound
// parameter of each query.
var dimensionExpressions = map[Dimension]string{
	DimensionRoute:              "route_slug",
	DimensionProvider:           "provider_id::text",
	DimensionModel:              "upstream_model",
	DimensionEndUser:            "COALESCE(NULLIF(end_user_digest, ''), 'unidentified')",
	DimensionProject:            projectDimensionExpression,
	DimensionSession:            sessionDimensionExpression,
	DimensionAPIKey:             "COALESCE(api_key_id::text, 'system')",
	DimensionOperation:          "operation",
	DimensionModelFamily:        modelFamilyExpression,
	DimensionEstimateProvenance: "COALESCE(NULLIF(estimate_provenance, ''), 'none')",
	DimensionAttribution:        "",
}

func parseDimension(raw string, f Filters) (Dimension, error) {
	dimension := Dimension(raw)
	if _, known := dimensionExpressions[dimension]; !known {
		return "", fail(FailureInvalidDimension,
			"Dimension must be route, provider, model, model_family, estimate_provenance, api_key, end_user, project, session, operation, or attribution.")
	}
	if dimension == DimensionAttribution && f.AttributionKey == nil {
		return "", fail(FailureInvalidFilter, "The attribution breakdown requires the attribution_key filter.")
	}
	return dimension, nil
}

func dimensionScope(dimension Dimension, f Filters) countScope {
	switch {
	case dimension == DimensionProvider && f.Model == nil:
		return scopeProvider
	case dimension == DimensionModel && f.ProviderID == nil:
		return scopeModel
	case dimension == DimensionProvider || dimension == DimensionModel:
		return scopeTarget
	default:
		return scopeFor(f)
	}
}

// Granularity is a time series bucket size, spelled as date_trunc spells it.
type Granularity string

const (
	GranularityHour Granularity = "hour"
	GranularityDay  Granularity = "day"
)

func parseGranularity(raw string) (Granularity, error) {
	switch granularity := Granularity(raw); granularity {
	case GranularityHour, GranularityDay:
		return granularity, nil
	default:
		return "", fail(FailureInvalidGranularity, "Granularity must be hour or day.")
	}
}

// ProjectScope is the set of projects a report may read. The zero value reads
// none, so forgetting to set one cannot widen a report.
type ProjectScope struct {
	all bool
	ids []string
}

func AllProjects() ProjectScope               { return ProjectScope{all: true} }
func OnlyProjects(ids ...string) ProjectScope { return ProjectScope{ids: ids} }
