package routes

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/contracts/interaction"
	mediacontract "github.com/tyk-swe/olp/internal/contracts/media"
	"github.com/tyk-swe/olp/internal/contracts/unary"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/media"
)

func (s *Server) inspectorAnyRequest(raw json.RawMessage, operation, surface, mode, dialect, slug string, strict bool) (*wire.Request, *oif.Request, *media.Request, error) {
	if !strict && !operationregistry.PolicyFor(operation).TransformedUnary || operation == "generation" {
		request, err := inspectorRequest(raw, operation, surface, mode, dialect, slug, strict)
		return request, nil, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, nil, nil
	}
	doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return nil, nil, nil, httpkit.Invalid("request", "Use one bounded unambiguous native request.")
	}
	tuple := len(doc.Root().Members()) > 0
	for _, member := range doc.Root().Members() {
		tuple = tuple && (member.Name == "model" || member.Name == "route")
	}
	if tuple {
		return nil, nil, nil, nil
	}
	if vendors.IsMediaOperation(operation) {
		if surface != "openai" || mode != "unary" && mode != "streaming" || media.Operation(operation) != media.OpImageGeneration && media.Operation(operation) != media.OpSpeech {
			return nil, nil, nil, httpkit.Invalid("request", "This media source requires a live multipart or lifecycle invocation; the inspector cannot stage its assets.")
		}
		var request *media.Request
		var failure *media.Error
		if media.Operation(operation) == media.OpImageGeneration {
			request, failure = media.DecodeImageGeneration(raw)
		} else {
			request, failure = media.DecodeSpeech(raw)
		}
		if failure != nil || request.Route() != slug || request.Mode() != mode {
			return nil, nil, nil, httpkit.Invalid("request", "Use a native media request naming this route and delivery mode.")
		}
		return nil, nil, request, nil
	}
	if dialect == "" {
		defaults := map[string]string{"openai/decision": "openai-decisions", "gemini/decision": "gemini-decisions", "openai/embeddings": "openai-embeddings", "openai/rerank": "rerank", "openai/moderation": "openai-moderation", "openai/token_count": "openai-input-tokens", "openai/ocr": "mistral-ocr", "openai/search": "perplexity-search", "anthropic/token_count": "anthropic-count-tokens", "gemini/token_count": "gemini-count-tokens", "gemini/embeddings": "gemini-embeddings", "bedrock/token_count": "bedrock-count-tokens"}
		dialect = defaults[surface+"/"+operation]
	}
	codec, ok := operationregistry.Lookup(dialect)
	if !ok || codec.Operation.ID != operation || mode != "unary" || surface != "native" && surface != codec.Surface {
		return nil, nil, nil, httpkit.Invalid("dialect", "Choose a registered unary dialect for this operation and surface.")
	}
	request, err := unary.Parse(dialect, raw, 1<<20)
	if err != nil {
		return nil, nil, nil, httpkit.Invalid("request", "Use a valid native operation source.")
	}
	if err := protocols.ValidateOperationInlineMedia(doc, codec.Identity, s.InlineMedia); err != nil {
		return nil, nil, nil, httpkit.Invalid("request", err.Error())
	}
	return nil, &request, nil, nil
}
func inspectionUnaryAccept(route routing.Route, sourceRoute string, source oif.Request, context interaction.Context, client string, demand *routing.TokenDemand) (routing.PrepareCandidate, map[string]*interactionInspection) {
	details := map[string]*interactionInspection{}
	prepare := func(provider routing.Provider, target routing.Target) (*routing.CandidatePreparation, error) {
		result := &interactionInspection{Status: "incompatible", Fidelity: routing.FidelityStrict, Evidence: []string{}}
		details[target.ID] = result
		template, err := unary.Compile(unary.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: source.Descriptor().Operation.ID, AllowProviderFetchedDocuments: route.ProviderFetchedDocuments})
		if err != nil {
			return nil, safeInspectionError(err)
		}
		plan, err := template.Bind(source, unary.Context{Route: sourceRoute, Headers: context.Headers, Query: context.Query, ClientContract: client})
		if err != nil {
			return nil, safeInspectionError(err)
		}
		receipt := plan.Receipt()
		result.Class, result.Operation = receipt.Class, receipt.Operation
		result.IngressDialect, result.EgressDialect, result.ReturnDialect = receipt.SourceDialect, receipt.TargetDialect, receipt.SourceDialect
		result.Representation = "oif"
		result.ProfileID, result.ProfileRevision = receipt.ProfileID, receipt.ProfileRevision
		result.Evidence = receipt.Evidence
		result.Serving = inspectServing(provider, plan.Serving())
		o := receipt.Obligations
		result.Obligations = &inspectedObligations{Delivery: o.Delivery, Lifetime: o.Lifetime, Submission: o.Submission, Effects: o.Effects, Continuation: o.Continuation, Retry: o.Retry, MaxBodyBytes: o.MaxBodyBytes, RejectAmbiguousFailover: o.RejectAmbiguousFailover, GuardResults: o.GuardResults}
		for _, d := range receipt.Dispositions {
			if len(result.Dispositions) < 64 {
				result.Dispositions = append(result.Dispositions, inspectedDisposition{inspectionField(d.Field), d.Disposition, d.Rule, d.Evidence})
			} else {
				result.OmittedDispositions++
			}
		}
		summary := inspectRequest(plan.Prepared().Document(), plan.Prepared().Provenance())
		result.EffectiveRequest = &summary
		result.Status = "admitted"
		return &routing.CandidatePreparation{Parameters: routing.Listed(plan.Parameters()), Demand: demand}, nil
	}
	return prepare, details
}

func inspectionMediaAccept(route routing.Route, source *media.Request, dialect string, context interaction.Context, client string, demand *routing.TokenDemand) (routing.PrepareCandidate, map[string]*interactionInspection) {
	details := map[string]*interactionInspection{}
	prepare := func(provider routing.Provider, target routing.Target) (*routing.CandidatePreparation, error) {
		result := &interactionInspection{Status: "incompatible", Fidelity: routing.FidelityStrict, Evidence: []string{}}
		details[target.ID] = result
		if client != "" || len(context.Headers) != 0 || len(context.Query) != 0 {
			return nil, safeInspectionError(&oif.Incompatibility{Code: "target_capability", Field: "/semantic_context", Requirement: "media_context", Message: "This media profile has no qualified caller semantic context."})
		}
		template, err := mediacontract.Compile(mediacontract.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: string(source.Op())})
		if err != nil {
			return nil, safeInspectionError(err)
		}
		call, effective, failure := media.EncodeStrictConfigured(source, provider.Connector(), target.ProviderModel)
		if failure != nil {
			return nil, safeInspectionError(failure)
		}
		input, err := media.ContractInput(source, call, effective)
		if err != nil {
			return nil, safeInspectionError(err)
		}
		bound, err := template.Bind(input)
		if err != nil {
			return nil, safeInspectionError(err)
		}
		receipt := bound.Receipt
		if dialect != "" && dialect != receipt.SourceDialect {
			return nil, safeInspectionError(&oif.Incompatibility{Code: "target_capability", Field: "/dialect", Requirement: "native_media_dialect", Message: "The requested media dialect differs from the selected target."})
		}
		result.Class, result.Operation = receipt.Class, receipt.Operation
		result.IngressDialect, result.EgressDialect, result.ReturnDialect = receipt.SourceDialect, receipt.TargetDialect, receipt.SourceDialect
		result.Representation = "oif"
		result.ProfileID, result.ProfileRevision = receipt.ProfileID, receipt.ProfileRevision
		result.Evidence = receipt.Evidence
		result.Serving = inspectServing(provider, receipt.Serving)
		o := receipt.Obligations
		result.Obligations = &inspectedObligations{Delivery: o.Delivery, Lifetime: o.Lifetime, Submission: o.Submission, Effects: o.Effects, Continuation: o.Continuation, Retry: o.Retry, MaxBodyBytes: o.MaxBodyBytes, MaxEventBytes: o.MaxEventBytes, RejectAmbiguousFailover: o.RejectAmbiguousFailover, GuardResults: o.GuardResults}
		for _, d := range receipt.Dispositions {
			if len(result.Dispositions) < 64 {
				result.Dispositions = append(result.Dispositions, inspectedDisposition{inspectionField(d.Field), d.Disposition, d.Rule, d.Evidence})
			} else {
				result.OmittedDispositions++
			}
		}
		summary := inspectRequest(bound.Effective.Document(), bound.Effective.Provenance())
		result.EffectiveRequest = &summary
		parameters := []string{}
		for _, member := range bound.Effective.Document().Root().Members() {
			if member.Name != "model" && member.Name != "stream" && member.Name != "stream_format" {
				parameters = append(parameters, member.Name)
			}
		}
		result.Status = "admitted"
		return &routing.CandidatePreparation{Parameters: routing.Listed(parameters), Demand: demand}, nil
	}
	return prepare, details
}
