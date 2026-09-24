package routes

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func inspectorAnyRequest(raw json.RawMessage, operation, surface, mode, dialect, slug string, strict bool) (*openai.Request, *oif.Request, *media.Request, error) {
	if !strict || operation == "generation" {
		request, err := inspectorRequest(raw, operation, surface, mode, dialect, slug, strict)
		return request, nil, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, nil, nil
	}
	doc, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		return nil, nil, nil, access.Invalid("request", "Use one bounded unambiguous native request.")
	}
	tuple := len(doc.Root().Members()) > 0
	for _, member := range doc.Root().Members() {
		tuple = tuple && (member.Name == "model" || member.Name == "route")
	}
	if tuple {
		return nil, nil, nil, nil
	}
	if mediacontract.IsMediaOperation(operation) {
		if surface != "openai" || mode != "unary" && mode != "streaming" || operation != "image_generation" && operation != "speech" {
			return nil, nil, nil, access.Invalid("request", "This media source requires a live multipart or lifecycle invocation; the inspector cannot stage its assets.")
		}
		var request *media.Request
		var failure *media.Error
		if operation == "image_generation" {
			request, failure = media.DecodeImageGeneration(raw)
		} else {
			request, failure = media.DecodeSpeech(raw)
		}
		if failure != nil || request.Route != slug || request.Mode() != mode {
			return nil, nil, nil, access.Invalid("request", "Use a native media request naming this route and delivery mode.")
		}
		return nil, nil, request, nil
	}
	if dialect == "" {
		defaults := map[string]string{"openai/embeddings": "openai-embeddings", "openai/rerank": "rerank", "openai/moderation": "openai-moderation", "openai/token_count": "openai-input-tokens", "anthropic/token_count": "anthropic-count-tokens", "gemini/token_count": "gemini-count-tokens", "gemini/embeddings": "gemini-embeddings", "bedrock/token_count": "bedrock-count-tokens"}
		dialect = defaults[surface+"/"+operation]
	}
	codec, ok := operationregistry.Lookup(dialect)
	if !ok || codec.Operation.ID != operation || mode != "unary" || surface != "native" && surface != codec.Surface {
		return nil, nil, nil, access.Invalid("dialect", "Choose a registered unary dialect for this operation and surface.")
	}
	request, err := operationplan.Parse(dialect, raw, 1<<20)
	if err != nil {
		return nil, nil, nil, access.Invalid("request", "Use a valid native operation source.")
	}
	return nil, &request, nil, nil
}
func inspectionUnaryAccept(route runtime.Route, source oif.Request, context interaction.Context, client string, demand *runtime.TokenDemand) (func(runtime.Provider, runtime.Target) error, func(runtime.Provider, runtime.Target) ([]string, *runtime.TokenDemand), map[string]*interactionInspection) {
	details := map[string]*interactionInspection{}
	plans := map[string]*operationplan.Plan{}
	accept := func(provider runtime.Provider, target runtime.Target) error {
		result := &interactionInspection{Status: "incompatible", Fidelity: runtime.FidelityStrict, Evidence: []string{}}
		details[target.ID] = result
		template, err := operationplan.Compile(operationplan.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: source.Descriptor().Operation.ID, Policy: route.ContentPolicy})
		if err != nil {
			return safeInspectionError(err)
		}
		plan, err := template.Bind(source, operationplan.Context{Route: route.Slug, Headers: context.Headers, Query: context.Query, ClientContract: client})
		if err != nil {
			return safeInspectionError(err)
		}
		receipt := plan.Receipt()
		result.Class, result.Operation = receipt.Class, receipt.Operation
		result.IngressDialect, result.EgressDialect, result.ReturnDialect = receipt.SourceDialect, receipt.TargetDialect, receipt.SourceDialect
		result.Representation = "oif"
		result.ProfileID, result.ProfileRevision = receipt.ProfileID, receipt.ProfileRevision
		result.Evidence = receipt.Evidence
		serving := plan.Serving()
		result.Serving = &inspectedServing{ProviderRevisionID: serving.RevisionID, Model: serving.Model, PrincipalDeclared: serving.PrincipalID != "", SnapshotDeclared: serving.Snapshot != "", RegionDeclared: serving.Region != "", ResourceScopeDeclared: serving.ResourceScope != ""}
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
		if _, err := plan.CheckInput(); err != nil {
			safe := safeInspectionError(err)
			if diagnostic, ok := safe.(*inspectionDiagnostic); ok && diagnostic.code == "content_policy_blocked" {
				result.Status = "blocked"
			}
			return safe
		}
		result.Status = "admitted"
		plans[target.ID] = plan
		return nil
	}
	effective := func(_ runtime.Provider, target runtime.Target) ([]string, *runtime.TokenDemand) {
		plan := plans[target.ID]
		if plan == nil {
			return nil, demand
		}
		return plan.Parameters(), demand
	}
	return accept, effective, details
}

func inspectionMediaAccept(route runtime.Route, source *media.Request, dialect string, context interaction.Context, client string, demand *runtime.TokenDemand) (func(runtime.Provider, runtime.Target) error, func(runtime.Provider, runtime.Target) ([]string, *runtime.TokenDemand), map[string]*interactionInspection) {
	details := map[string]*interactionInspection{}
	parameters := map[string][]string{}
	accept := func(provider runtime.Provider, target runtime.Target) error {
		result := &interactionInspection{Status: "incompatible", Fidelity: runtime.FidelityStrict, Evidence: []string{}}
		details[target.ID] = result
		if client != "" || len(context.Headers) != 0 || len(context.Query) != 0 {
			return safeInspectionError(&oif.Incompatibility{Code: "target_capability", Field: "/semantic_context", Requirement: "media_context", Message: "This media profile has no qualified caller semantic context."})
		}
		template, err := mediacontract.Compile(mediacontract.Config{Provider: provider.Connector(), ProviderID: provider.ID, RevisionID: provider.RevisionID, Model: target.ProviderModel, Operation: source.Op, Policy: route.ContentPolicy})
		if err != nil {
			return safeInspectionError(err)
		}
		call, effective, failure := media.EncodeStrictConfigured(source, provider.Connector(), target.ProviderModel)
		if failure != nil {
			return safeInspectionError(failure)
		}
		bound, err := template.Bind(mediacontract.Input{Route: route.Slug, Mode: effective.Mode(), Source: source.SourceDocument(), JSON: call.JSON})
		if err != nil {
			safe := safeInspectionError(err)
			if diagnostic, ok := safe.(*inspectionDiagnostic); ok && diagnostic.code == "content_policy_blocked" {
				result.Status = "blocked"
			}
			return safe
		}
		receipt := bound.Receipt
		if dialect != "" && dialect != receipt.SourceDialect {
			return safeInspectionError(&oif.Incompatibility{Code: "target_capability", Field: "/dialect", Requirement: "native_media_dialect", Message: "The requested media dialect differs from the selected target."})
		}
		result.Class, result.Operation = receipt.Class, receipt.Operation
		result.IngressDialect, result.EgressDialect, result.ReturnDialect = receipt.SourceDialect, receipt.TargetDialect, receipt.SourceDialect
		result.Representation = "oif"
		result.ProfileID, result.ProfileRevision = receipt.ProfileID, receipt.ProfileRevision
		result.Evidence = receipt.Evidence
		serving := receipt.Serving
		result.Serving = &inspectedServing{ProviderRevisionID: serving.RevisionID, Model: serving.Model, PrincipalDeclared: serving.PrincipalID != "", SnapshotDeclared: serving.Snapshot != "", RegionDeclared: serving.Region != "", ResourceScopeDeclared: serving.ResourceScope != ""}
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
		for _, member := range bound.Effective.Document().Root().Members() {
			if member.Name != "model" && member.Name != "stream" && member.Name != "stream_format" {
				parameters[target.ID] = append(parameters[target.ID], member.Name)
			}
		}
		result.Status = "admitted"
		return nil
	}
	effective := func(_ runtime.Provider, target runtime.Target) ([]string, *runtime.TokenDemand) {
		return parameters[target.ID], demand
	}
	return accept, effective, details
}
