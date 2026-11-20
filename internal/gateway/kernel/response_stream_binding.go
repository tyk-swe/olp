package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/resources"
)

var errResponseMapping = &wire.ProtocolError{Detail: "stored response mapping failed"}

type responseLocalRefusal struct{ cause error }

func (e *responseLocalRefusal) Error() string {
	return "stored response projection or persistence failed"
}

func (e *responseLocalRefusal) Unwrap() error { return e.cause }

type responseStreamBinding struct {
	attemptOrdinal int
	upstreamID     string
	localID        string
}

func (x *Call) responseStreamIdentity(ordinal int, upstreamID string) (*responseStreamBinding, error) {
	if x.generation.responseBinding == nil || x.generation.responseBinding.attemptOrdinal != ordinal {
		x.generation.responseBinding = &responseStreamBinding{attemptOrdinal: ordinal, upstreamID: upstreamID}
	}
	if x.generation.responseBinding.upstreamID != upstreamID {
		return nil, errResponseMapping
	}
	return x.generation.responseBinding, nil
}

func (binding *responseStreamBinding) mappedID(ordinal int, upstreamID string) (string, bool) {
	if binding == nil || binding.attemptOrdinal != ordinal || binding.upstreamID != upstreamID || binding.localID == "" {
		return "", false
	}
	return binding.localID, true
}

func (s *Server) observeStreamResponseEvent(ctx context.Context, x *Call, fact *events.AttemptFact, event oif.Event) error {
	if s.Deps.Resources == nil || !responseStoreRequested(x.generation.parsed) {
		return nil
	}
	observation, lifecycle, err := openai.ObserveResponseEventUsage(event)
	if err != nil {
		return errResponseMapping
	}
	if !lifecycle {
		return nil
	}
	response, present := event.Source().Root().Lookup("response")
	if !present || response.Kind() == oif.Null {
		if x.Strict() {
			return errResponseMapping
		}
		return nil
	}
	if response.Kind() != oif.Object {
		return errResponseMapping
	}
	id, present := response.Lookup("id")
	upstreamID, valid := id.Text()
	if !present || !valid || upstreamID == "" || len(upstreamID) > 512 ||
		strings.HasPrefix(upstreamID, resources.KindResponse+"_") || strings.HasPrefix(upstreamID, resources.KindStrictResponse+"_") {
		return errResponseMapping
	}
	binding, err := x.responseStreamIdentity(fact.Ordinal, upstreamID)
	if err != nil {
		return err
	}
	local := binding.localID
	if local == "" {
		commitCtx, stopCommit := ResourceCommitContext(ctx)
		res, err := s.Deps.Resources.GetByUpstream(commitCtx, responseNativeIdentity(x, fact, upstreamID))
		if errors.Is(err, resources.ErrNotFound) {
			res, err = s.putStreamResponse(commitCtx, x, fact, upstreamID, observation.Status, response)
		}
		stopCommit()
		if err != nil {
			return &responseLocalRefusal{cause: err}
		}
		local = res.ID
		if backgroundResponseRequested(x.generation.parsed) {
			if err := attachGenerationHTTP(x, res); err != nil {
				return &responseLocalRefusal{cause: err}
			}
			fact.GenerationUsageDeferred = true
		}
		binding.localID = local
	}
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	settleErr := s.applyResponseObservation(commitCtx, local, observation)
	stopCommit()
	if settleErr != nil {
		s.Log.Warn("retained response observation persistence failed", "error", settleErr)
		return &responseLocalRefusal{cause: settleErr}
	}
	return nil
}

func (s *Server) mapStreamResponseFrame(ctx context.Context, x *Call, fact *events.AttemptFact, frame []byte, observe bool) ([]byte, error) {
	if s.Deps.Resources == nil || !responseStoreRequested(x.generation.parsed) {
		return frame, nil
	}
	i := bytes.Index(frame, []byte("\ndata: "))
	if i < 0 {
		return frame, nil
	}
	payload := bytes.TrimSpace(frame[i+7:])
	if len(payload) == 0 || payload[0] != '{' {
		return frame, nil
	}
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: int(s.Config.MaxEventBytes)})
	if err != nil || doc.Root().Kind() != oif.Object {
		return nil, errResponseMapping
	}
	nativeEvent, err := oif.NewEvent(wire.Descriptor(wire.Responses, true), doc, "", 0)
	if err != nil {
		return nil, errResponseMapping
	}
	_, lifecycle, err := openai.ObserveResponseEventUsage(nativeEvent)
	if err != nil {
		return nil, errResponseMapping
	}
	if !lifecycle {
		return frame, nil
	}
	if observe {
		if err := s.observeStreamResponseEvent(ctx, x, fact, nativeEvent); err != nil {
			return nil, err
		}
	}
	response, present := doc.Root().Lookup("response")
	if !present || response.Kind() == oif.Null {
		if x.Strict() {
			if kind, found := doc.Root().Lookup("type"); found {
				if name, valid := kind.Text(); valid && (name == "response.created" || name == "response.in_progress" || name == "response.queued") {
					return nil, errResponseMapping
				}
			}
		}
		return frame, nil
	}
	if response.Kind() != oif.Object {
		return nil, errResponseMapping
	}
	id, present := response.Lookup("id")
	upstreamID, valid := id.Text()
	if !present || !valid || upstreamID == "" || len(upstreamID) > 512 ||
		strings.HasPrefix(upstreamID, resources.KindResponse+"_") || strings.HasPrefix(upstreamID, resources.KindStrictResponse+"_") {
		return nil, errResponseMapping
	}
	local, ok := x.generation.responseBinding.mappedID(fact.Ordinal, upstreamID)
	if !ok {
		return nil, &responseLocalRefusal{cause: errResponseMapping}
	}
	encoded, err := json.Marshal(local)
	if err != nil {
		return nil, errResponseMapping
	}
	var mapped oif.Document
	if x.Strict() {
		projection := responseProjection{upstreamID: upstreamID, localID: local, route: x.Route.Slug}
		if previous := x.previousResponse(); previous != nil {
			projection.previousUpstream, projection.previousLocal = previous.UpstreamID, previous.ID
		}
		mapped, err = projection.project(doc, "/response")
	} else {
		changes := []oif.Change{{Pointer: "/response/id", Value: string(encoded), Origin: oif.ResourceBinding, Reason: "owner-scoped retained response"}}
		if previous, present := response.Lookup("previous_response_id"); present {
			if upstream, valid := previous.Text(); valid && upstream != "" {
				value := "null"
				commitCtx, stopCommit := ResourceCommitContext(ctx)
				if local := s.previousResponseLocal(commitCtx, responseNativeIdentity(x, fact, ""), x.pin, upstream); local != "" {
					value = quotedResponseID(local)
				}
				stopCommit()
				changes = append(changes, oif.Change{Pointer: "/response/previous_response_id", Value: value, Origin: oif.ResourceBinding, Reason: "owner-scoped prior response"})
			}
		}
		mapped, err = oif.Apply(doc, changes)
		if err != nil {
			return nil, &responseLocalRefusal{cause: err}
		}
	}
	if err != nil {
		if _, local := errors.AsType[*responseLocalRefusal](err); local {
			return nil, err
		}
		return nil, errResponseMapping
	}
	body := mapped.Bytes()
	out := make([]byte, 0, i+7+len(body)+2)
	out = append(out, frame[:i+7]...)
	out = append(out, body...)
	out = append(out, '\n', '\n')
	return out, nil
}

func (s *Server) putStreamResponse(ctx context.Context, x *Call, fact *events.AttemptFact, upstreamID, state string, response oif.Value) (*resources.Resource, error) {
	metadata := map[string]json.RawMessage{}
	for _, name := range responseMetadataFields() {
		if value, present := response.Lookup(name); present {
			metadata[name] = value.Bytes()
		}
	}
	if fact.UpstreamModel != "" {
		encoded, _ := json.Marshal(fact.UpstreamModel)
		metadata["upstream_model"] = encoded
	}
	if state == "" {
		state = "created"
	}
	var expires *time.Time
	if value, present := response.Lookup("expires_at"); present {
		var seconds int64
		if json.Unmarshal(value.Bytes(), &seconds) == nil && seconds > 0 {
			at := time.Unix(seconds, 0).UTC()
			expires = &at
		}
	}
	var credential *string
	if fact.CredentialID != "" {
		credential = &fact.CredentialID
	}
	deferred := s.pendingResponseUsage(x, fact, metadata)
	encoded, _ := json.Marshal(metadata)
	if x.Strict() {
		return s.putStrictResponse(ctx, x, fact, upstreamID, state, encoded, expires)
	}
	res, err := s.Deps.Resources.Put(ctx, &resources.Resource{
		Kind:               resources.KindResponse,
		APIKeyID:           x.keyID,
		RouteSlug:          x.Route.Slug,
		ProviderID:         fact.ProviderID,
		ProviderRevisionID: fact.ProviderRevisionID,
		RouteRevisionID:    x.Route.RevisionID,
		TargetID:           fact.TargetID,
		SlotID:             fact.SlotID,
		CredentialID:       credential,
		UpstreamID:         upstreamID,
		State:              state,
		Metadata:           encoded,
		ExpiresAt:          expires,
	})
	if err == nil && deferred {
		fact.GenerationUsageDeferred = true
	}
	return res, err
}
