package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/resources"
)

type resultOwner struct {
	identity resources.NativeIdentity
	storedAt *time.Time
}

func liveResult(identity resources.NativeIdentity) resultOwner {
	return resultOwner{identity: identity}
}

func storedResult(response *resources.Resource) resultOwner {
	return resultOwner{identity: response.NativeIdentity(), storedAt: &response.CreatedAt}
}

func (o resultOwner) find(ctx context.Context, store *resources.Store, kind, upstream string) (*resources.Resource, error) {
	identity := o.identity
	identity.Kind, identity.UpstreamID, identity.ParentID = kind, upstream, nil
	if o.storedAt == nil {
		return store.GetByUpstreamIncludingDeleted(ctx, identity)
	}
	return store.GetByUpstreamAsOf(ctx, identity, *o.storedAt)
}

func (s *Server) StoredGenerationResultResolver(ctx context.Context, x *Call, response *resources.Resource) generationrefs.Resolve {
	return s.generationResultResolver(ctx, x, storedResult(response))
}

func (s *Server) generationResultResolver(ctx context.Context, x *Call, owner resultOwner) generationrefs.Resolve {
	return func(kind, upstream string) (string, error) {
		key := kind + "\x00" + upstream
		if x.generation.resources != nil {
			if bound := x.generation.resources.bound[key]; bound != nil {
				return bound.ID, nil
			}
		}
		if s.Deps.Resources == nil {
			return "", resources.ErrNotFound
		}
		resource, err := owner.find(ctx, s.Deps.Resources, kind, upstream)
		if errors.Is(err, resources.ErrNotFound) && kind == resources.KindFile {
			resource, err = owner.find(ctx, s.Deps.Resources, resources.KindStrictFile, upstream)
		}
		if err != nil {
			return "", err
		}
		if x.generation.resources != nil {
			x.generation.resources.bound[key] = resource
		}
		return resource.ID, nil
	}
}

func (s *Server) mapGenerationResultReferences(ctx context.Context, x *Call, owner resultOwner, body []byte) ([]byte, error) {
	return generationrefs.ProjectBody(body, oif.Limits{MaxBytes: int(s.Config.MaxResponseBytes)}, s.generationResultResolver(ctx, x, owner))
}

func (s *Server) mapGenerationReferenceFrame(ctx context.Context, x *Call, owner resultOwner, frame []byte) ([]byte, error) {
	return generationrefs.ProjectFrame(frame, oif.Limits{MaxBytes: int(s.Config.MaxEventBytes)}, s.generationResultResolver(ctx, x, owner))
}
