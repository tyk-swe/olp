package retainedworker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

type childPathSpec struct {
	parentSegment string
	childSegment  string
}

func (w *Worker) path(ctx context.Context, r *resources.Resource, upstream string) (string, retainedapi.Endpoint, error) {
	family, ok := lookupFamily(r.Kind)
	endpoint := retainedapi.Endpoint{Family: family, Action: "get", Method: http.MethodGet}
	if !ok {
		return "", endpoint, errors.New("unknown retained family")
	}
	if spec, isChild := childSpec(r.Kind); isChild {
		resourcePath, err := w.childPath(ctx, r, upstream, spec)
		return resourcePath, endpoint, err
	}
	resourcePath, err := topLevelPath(r.Kind, upstream)
	return resourcePath, endpoint, err
}

func lookupFamily(kind string) (retainedapi.Family, bool) {
	for _, f := range retainedapi.Families() {
		if f.Kind == kind {
			return f, true
		}
		for _, child := range f.Children {
			if child.Kind == kind {
				matched := child
				matched.Operation = f.Operation
				matched.Surface = f.Surface
				return matched, true
			}
		}
	}
	return retainedapi.Family{}, false
}

func topLevelPath(kind, upstream string) (string, error) {
	switch kind {
	case "fine_tuning_job":
		return "fine_tuning/jobs/" + url.PathEscape(upstream), nil
	case "vertex_tuning_job":
		parts := strings.Split(upstream, "/tuningJobs/")
		return "tuningJobs/" + url.PathEscape(parts[len(parts)-1]), nil
	case "vector_store":
		return "vector_stores/" + url.PathEscape(upstream), nil
	case "container":
		return "containers/" + url.PathEscape(upstream), nil
	case "context_cache":
		return "cachedContents/" + url.PathEscape(strings.TrimPrefix(upstream, "cachedContents/")), nil
	default:
		return "", errors.New("unqualified retained observation")
	}
}

func childSpec(kind string) (childPathSpec, bool) {
	switch kind {
	case "eval_run":
		return childPathSpec{parentSegment: "evals", childSegment: "runs"}, true
	case "vector_store_file":
		return childPathSpec{parentSegment: "vector_stores", childSegment: "files"}, true
	case "vector_store_batch":
		return childPathSpec{parentSegment: "vector_stores", childSegment: "file_batches"}, true
	case "container_file":
		return childPathSpec{parentSegment: "containers", childSegment: "files"}, true
	default:
		return childPathSpec{}, false
	}
}

func (w *Worker) childPath(ctx context.Context, r *resources.Resource, upstream string, spec childPathSpec) (string, error) {
	if r.ParentID == nil {
		return "", errors.New("missing parent")
	}
	lookup := w.resourceClient()
	if lookup == nil {
		return "", errors.New("resource store unavailable")
	}
	parent, err := lookup.GetByUUID(ctx, *r.ParentID, r.APIKeyID)
	if err != nil {
		return "", err
	}
	return spec.parentSegment + "/" + url.PathEscape(parent.UpstreamID) + "/" + spec.childSegment + "/" + url.PathEscape(upstream), nil
}
