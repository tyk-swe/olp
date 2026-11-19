package geministate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) geminiCreateBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	slug := r.PathValue("model")
	if header := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader)); header != "" && header != slug {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch model path must match X-OLP-Route.", nil))
		return
	}
	route, err := s.kernel.NativeStateRoute(x, authority, r, slug)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	body, err := s.kernel.ReadResourceBody(w, r)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	input, err := validateGeminiBatchRequest(body, route.Slug)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	p, pinnedRoute, file, err := s.geminiBatchPin(r.Context(), x, authority, route, input)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	if pinnedRoute != nil {
		route = pinnedRoute
	}
	x.Route = route
	if file != nil {
		var metadata struct {
			Manifest kernel.BatchManifest `json:"batch_manifest"`
		}
		if json.Unmarshal(file.Metadata, &metadata) != nil || metadata.Manifest.Adapter != kernel.GeminiNativeManifest || metadata.Manifest.LineCount < 1 || metadata.Manifest.ManifestDigest == "" {
			s.kernel.ReleaseHold(r.Context(), p.Hold)
			s.kernel.Refuse(x, w, kernel.PolicyUnavailable("target_capability", "File-backed Gemini batches require qualified JSONL uploaded in one complete final chunk."))
			return
		}
		input.Expected = new(int64(metadata.Manifest.LineCount))

		input.UpstreamFile = "files/" + file.UpstreamID
		rewritten, rewriteErr := rewriteGeminiBatchInputFile(input.Upstream, input.UpstreamFile)
		if rewriteErr != nil {
			s.kernel.ReleaseHold(r.Context(), p.Hold)
			s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The input file name could not be bound.", nil))
			return
		}
		input.Upstream = rewritten
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.geminiResourceGates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	source, sourceErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if sourceErr != nil {
		s.kernel.Refuse(x, w, fault.InvalidRequest("invalid_request", "The Gemini batch request is invalid.", nil))
		return
	}
	localInputFile, upstreamFile := geminiInputFiles(file)
	endpoint, endpointErr := kernel.ResourceURL(p.Provider.Connector(), p.Model, "models/"+url.PathEscape(p.Provider.Connector().Model(p.Model))+":batchGenerateContent", nil)
	if endpointErr != nil {
		s.kernel.Refuse(x, w, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved."))
		return
	}
	submission, e := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		Kind: resources.KindGeminiBatch, Adapter: connectors.BatchGemini, Surface: batches.SurfaceGemini,
		Operation: wire.LineGeneration, Window: 48 * time.Hour, Expected: input.Expected,
		Submission: geminiSubmission(localInputFile, upstreamFile), Source: source, Canonical: input.Canonical, Effective: input.Upstream,
	})
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	id := submission.Resource.UUID
	result, exchange, e := s.kernel.SendBatch(ctx, x, p, id, endpoint, input.Upstream)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	batch, decodeErr := upstream.DecodeGeminiBatch(result)
	if decodeErr != nil {
		s.kernel.Refuse(x, w, s.kernel.BatchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	object, metadata, metadataErr := settleGeminiBatchResult(exchange, result, batch, p.Model, geminiInputNames{Local: localInputFile, Upstream: upstreamFile})
	if metadataErr != nil {
		s.kernel.Refuse(x, w, s.kernel.BatchSubmissionUnknown(ctx, id, "The provider returned an invalid batch object. The batch may have been accepted."))
		return
	}
	resource, e := s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: batch.ID, State: batch.State, Metadata: metadata})
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	projected, projectErr := s.projectGeminiBatchResource(ctx, resource, object)
	if projectErr != nil {
		s.kernel.Refuse(x, w, geminiBatchProjectionError(projectErr))
		return
	}
	s.kernel.WriteStateJSON(w, x, projected, exchange)
}

func geminiInputFiles(file *resources.Resource) (string, string) {
	if file == nil {
		return "", ""
	}
	return "files/" + file.ID, "files/" + file.UpstreamID
}

func geminiSubmission(localInputFile, upstreamFile string) batches.Submission {
	if upstreamFile == "" {
		return batches.GeminiInlineSubmission{}
	}
	return batches.GeminiFileSubmission{GeminiFile: upstreamFile, LocalInputFileID: localInputFile}
}

func (s surface) geminiBatchPin(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, input geminiBatchInput) (*kernel.Pin, *routing.Route, *resources.Resource, *fault.Error) {
	if input.LocalFileID == "" {
		p, err := s.kernel.NativeStatePin(ctx, x, route)
		return p, route, nil, err
	}
	file, err := s.kernel.Deps.Resources.Get(ctx, resources.KindGeminiFile, authority.ID, input.LocalFileID)
	if err != nil {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	if file.RouteSlug != route.Slug {
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	p, pinnedRoute, pinErr := s.kernel.ResolveResource(ctx, x, authority, file, "batch", kernel.RetainedNewWork)
	if pinErr != nil || pinnedRoute == nil || pinnedRoute.Slug != route.Slug {
		if pinErr != nil && pinErr.Code != "not_found" {
			return nil, nil, nil, pinErr
		}
		return nil, nil, nil, fault.NotFoundError("not_found", "No Gemini file with this identifier exists for this key.")
	}
	return p, pinnedRoute, file, nil
}
