package kernel

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/upstream"
)

// Generic retained families share the managed-agent lifecycle contracts:
// owner-scoped gateway IDs, pinned route/provider/credential affinity,
// submission fences for creates, and no local content retention. Each
// family keeps its native wire shape; only identity fields are rewritten.

func retainedSurfaceFormat(surface string) wire.Format {
	switch surface {
	case "anthropic":
		return wire.AnthropicFile
	case "gemini", "native":
		return wire.GeminiFile
	}
	return wire.File
}

func (s *Server) registerRetainedGeneric(mux *http.ServeMux) {
	for _, endpoint := range retainedapi.Endpoints() {
		if endpoint.Operation == "agent_resource" {
			continue
		}
		e := endpoint
		mux.HandleFunc(e.Method+" "+e.Pattern, func(w http.ResponseWriter, r *http.Request) { s.retainedGeneric(w, r, e) })
	}
	mux.HandleFunc("POST /v1/vector_stores/{id}/search", s.retainedSearch)
	mux.HandleFunc("POST /v1/responses/compact", s.retainedCompact)
}

// upstreamPath maps a gateway pattern to the provider-relative path by
// substituting upstream IDs and stripping the gateway surface prefix.
func retainedUpstreamPath(e retainedapi.Endpoint, ids map[string]string) string {
	path := e.Pattern
	for name, id := range ids {
		if e.Surface == "gemini" {
			id = strings.TrimPrefix(id, "cachedContents/")
		}
		if e.Surface == "native" {
			if i := strings.LastIndex(id, "/tuningJobs/"); i >= 0 {
				id = id[i+len("/tuningJobs/"):]
			}
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(id))
	}
	switch e.Surface {
	case "anthropic":
		path = strings.TrimPrefix(path, "/anthropic/v1/")
	case "gemini":
		path = strings.TrimPrefix(path, "/gemini/v1beta/")
	case "native":
		path = strings.TrimPrefix(path, "/native/vertex-tuning/")
		path = "tuningJobs" + strings.TrimPrefix(path, "jobs")
		if e.Action == "cancel" {
			path = strings.TrimSuffix(path, "/cancel") + ":cancel"
		}
	default:
		path = strings.TrimPrefix(path, "/v1/")
	}
	return path
}

func retainedRejectedWithoutWork(failure *AttemptFailure) bool {
	if failure == nil {
		return false
	}
	if failure.Acceptance == upstream.NotSent {
		return true
	}
	if failure.Acceptance != upstream.Terminal || failure.accepted {
		return false
	}
	switch failure.Status {
	case 400, 401, 403, 404, 405, 413, 415, 422, 429:
		return true
	}
	return false
}
