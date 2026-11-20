package kernel

import (
	"bytes"
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/generationrefs"
	"github.com/tyk-swe/olp/internal/resources"
)

func (s *Server) generationResultResolver(ctx context.Context, x *Call, identity resources.NativeIdentity) generationrefs.Resolve {
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
		identity.Kind, identity.UpstreamID, identity.ParentID = kind, upstream, nil
		resource, err := s.Deps.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
		if errors.Is(err, resources.ErrNotFound) && kind == resources.KindFile {
			identity.Kind = resources.KindStrictFile
			resource, err = s.Deps.Resources.GetByUpstreamIncludingDeleted(ctx, identity)
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

func (s *Server) mapGenerationResultReferences(ctx context.Context, x *Call, identity resources.NativeIdentity, body []byte) ([]byte, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(s.Config.MaxResponseBytes)})
	if err != nil {
		return nil, err
	}
	projected, err := generationrefs.ProjectResult(doc, s.generationResultResolver(ctx, x, identity))
	if err != nil {
		return nil, err
	}
	return projected.Bytes(), nil
}

func (s *Server) mapGenerationReferenceFrame(ctx context.Context, x *Call, identity resources.NativeIdentity, frame []byte) ([]byte, error) {
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame, nil
	}
	payload := bytes.TrimSpace(frame[i+7:])
	if len(payload) == 0 || payload[0] != '{' {
		return frame, nil
	}
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: int(s.Config.MaxEventBytes)})
	if err != nil {
		return nil, err
	}
	projected, err := generationrefs.ProjectResult(doc, s.generationResultResolver(ctx, x, identity))
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), frame[:i+7]...)
	out = append(out, projected.Bytes()...)
	out = append(out, '\n', '\n')
	return out, nil
}
