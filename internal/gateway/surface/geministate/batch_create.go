package geministate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) createBatch(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.StateBegin(w, r, wire.GeminiBatch)
	if done {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	requested, body, input, err := s.batchRequest(w, r, x, authority)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	p, route, file, err := s.batchPin(r.Context(), x, authority, requested, input)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	x.Route = route
	bound, err := bindInputFile(input, file)
	if err != nil {
		s.kernel.ReleaseHold(r.Context(), p.Hold)
		s.kernel.Refuse(x, w, err)
		return
	}
	ctx, cancel := s.kernel.StateDeadline(r.Context(), route)
	defer cancel()
	defer s.kernel.ResourceSettle(ctx, x, p)
	if err := s.gates(ctx, x, authority, p, route, true); err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	reply, exchange, err := s.submit(ctx, x, authority, p, body, bound, file)
	if err != nil {
		s.kernel.Refuse(x, w, err)
		return
	}
	s.kernel.WriteStateJSON(w, x, reply, exchange)
}

func (s surface) batchRequest(w http.ResponseWriter, r *http.Request, x *kernel.Call, authority keyauthority.Authority) (*routing.Route, []byte, batchInput, *fault.Error) {
	slug := r.PathValue("model")
	if header := strings.TrimSpace(r.Header.Get(kernel.StateRouteHeader)); header != "" && header != slug {
		return nil, nil, batchInput{}, fault.InvalidRequest("invalid_request", "The Gemini batch model path must match X-OLP-Route.", nil)
	}
	route, err := s.kernel.NativeStateRoute(x, authority, r, slug)
	if err != nil {
		return nil, nil, batchInput{}, err
	}
	body, err := s.kernel.ReadResourceBody(w, r)
	if err != nil {
		return nil, nil, batchInput{}, err
	}
	input, err := validateBatch(body, route.Slug)
	return route, body, input, err
}

func bindInputFile(input batchInput, file *resources.Resource) (batchInput, *fault.Error) {
	if file == nil {
		return input, nil
	}
	var metadata struct {
		Manifest kernel.BatchManifest `json:"batch_manifest"`
	}
	if json.Unmarshal(file.Metadata, &metadata) != nil || metadata.Manifest.Adapter != kernel.GeminiNativeManifest || metadata.Manifest.LineCount < 1 || metadata.Manifest.ManifestDigest == "" {
		return batchInput{}, kernel.PolicyUnavailable("target_capability", "File-backed Gemini batches require qualified JSONL uploaded in one complete final chunk.")
	}
	upstreamFile := "files/" + file.UpstreamID
	rewritten, err := rewriteInputFile(input.Upstream, upstreamFile)
	if err != nil {
		return batchInput{}, fault.InvalidRequest("invalid_request", "The input file name could not be bound.", nil)
	}
	return batchInput{
		Canonical: input.Canonical, Upstream: rewritten, Expected: new(int64(metadata.Manifest.LineCount)), Keys: input.Keys,
		LocalFileID: input.LocalFileID, UpstreamFile: upstreamFile, DisplayName: input.DisplayName,
	}, nil
}

func (s surface) submit(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, p *kernel.Pin, body []byte, input batchInput, file *resources.Resource) ([]byte, *kernel.PinnedExchange, *fault.Error) {
	source, sourceErr := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if sourceErr != nil {
		return nil, nil, fault.InvalidRequest("invalid_request", "The Gemini batch request is invalid.", nil)
	}
	localInputFile, upstreamFile := inputFiles(file)
	endpoint, endpointErr := kernel.ResourceURL(p.Provider.Connector(), p.Model, "models/"+url.PathEscape(p.Provider.Connector().Model(p.Model))+":batchGenerateContent", nil)
	if endpointErr != nil {
		return nil, nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The Gemini batch address could not be resolved.")
	}
	submission, err := s.kernel.BeginBatch(ctx, x, authority.ID, p, kernel.BatchIntent{
		Kind: resources.KindGeminiBatch, Adapter: connectors.BatchGemini, Surface: batches.SurfaceGemini,
		Operation: wire.LineGeneration, Window: 48 * time.Hour, Expected: input.Expected,
		Submission: batchSubmission(localInputFile, upstreamFile), Source: source, Canonical: input.Canonical, Effective: input.Upstream,
	})
	if err != nil {
		return nil, nil, err
	}
	id := submission.Resource.UUID
	result, exchange, err := s.kernel.SendBatch(ctx, x, p, id, endpoint, input.Upstream)
	if err != nil {
		return nil, nil, err
	}
	const unknown = "The provider returned an invalid batch object. The batch may have been accepted."
	batch, decodeErr := upstream.DecodeGeminiBatch(result)
	if decodeErr != nil {
		return nil, nil, s.kernel.BatchSubmissionUnknown(ctx, id, unknown)
	}
	object, metadata, metadataErr := settleResult(exchange, result, batch, p.Model, inputNames{Local: localInputFile, Upstream: upstreamFile})
	if metadataErr != nil {
		return nil, nil, s.kernel.BatchSubmissionUnknown(ctx, id, unknown)
	}
	resource, err := s.kernel.ActivateBatch(ctx, submission, kernel.AcceptedBatch{UpstreamID: batch.ID, State: batch.State, Metadata: metadata})
	if err != nil {
		return nil, nil, err
	}
	projected, projectErr := s.projectResource(ctx, resource, object)
	if projectErr != nil {
		return nil, nil, projectionError(projectErr)
	}
	return projected, exchange, nil
}

func inputFiles(file *resources.Resource) (string, string) {
	if file == nil {
		return "", ""
	}
	return "files/" + file.ID, "files/" + file.UpstreamID
}

func batchSubmission(localInputFile, upstreamFile string) batches.Submission {
	if upstreamFile == "" {
		return batches.GeminiInlineSubmission{}
	}
	return batches.GeminiFileSubmission{GeminiFile: upstreamFile, LocalInputFileID: localInputFile}
}

func (s surface) batchPin(ctx context.Context, x *kernel.Call, authority keyauthority.Authority, route *routing.Route, input batchInput) (*kernel.Pin, *routing.Route, *resources.Resource, *fault.Error) {
	if input.LocalFileID == "" {
		p, err := s.kernel.NativeStatePin(ctx, x, route)
		return p, route, nil, err
	}
	file, err := s.store.Get(ctx, resources.KindGeminiFile, authority.ID, input.LocalFileID)
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
