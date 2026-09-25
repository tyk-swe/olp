// Package geminilifecycle validates the two distinct native Gemini lifecycle
// dialects. Its immutable JSON views preserve provider extension bytes and
// numeric lexemes; only model and resource identities receive overlays.
package geminilifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

const Revision = "v1beta"

type InteractionRequest struct {
	Source     oif.Document
	Model      string
	Previous   string
	Store      bool
	Stream     bool
	Background bool
}

func ParseInteractionRequest(body []byte, maxBytes int) (InteractionRequest, error) {
	if maxBytes < 1 || len(body) > maxBytes {
		return InteractionRequest{}, errors.New("interaction request exceeds byte limit")
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return InteractionRequest{}, errors.New("interaction request must be a bounded JSON object")
	}
	model, ok := stringField(doc.Root(), "model")
	if !ok || model == "" || len(model) > 128 {
		return InteractionRequest{}, errors.New("interaction model must name a route")
	}
	input, ok := doc.Root().Lookup("input")
	if !ok || input.Kind() == oif.Null {
		return InteractionRequest{}, errors.New("interaction input is required")
	}
	store, err := boolField(doc.Root(), "store", true)
	if err != nil {
		return InteractionRequest{}, err
	}
	stream, err := boolField(doc.Root(), "stream", false)
	if err != nil {
		return InteractionRequest{}, err
	}
	background, err := boolField(doc.Root(), "background", false)
	if err != nil {
		return InteractionRequest{}, err
	}
	previous := ""
	if v, present := doc.Root().Lookup("previous_interaction_id"); present {
		if previous, ok = v.Text(); !ok || previous == "" || len(previous) > 512 {
			return InteractionRequest{}, errors.New("previous_interaction_id must be a bounded string")
		}
	}
	if background && !store {
		return InteractionRequest{}, errors.New("background interactions require store=true")
	}
	if previous != "" && !store {
		return InteractionRequest{}, errors.New("previous_interaction_id requires store=true")
	}
	return InteractionRequest{Source: doc, Model: model, Previous: previous, Store: store, Stream: stream, Background: background}, nil
}

func (r InteractionRequest) Bind(model, previous string) ([]byte, error) {
	changes := []oif.Change{{Pointer: "/model", Value: quoted(model), Origin: oif.IdentityBinding, Reason: "qualified Gemini serving model"}}
	if r.Previous != "" {
		changes = append(changes, oif.Change{Pointer: "/previous_interaction_id", Value: quoted(previous), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini interaction"})
	}
	bound, err := oif.Apply(r.Source, changes)
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}

type InteractionResponse struct {
	Source oif.Document
	ID     string
	Status string
}

func ParseInteractionResponse(body []byte, maxBytes int) (InteractionResponse, error) {
	if maxBytes < 1 || len(body) > maxBytes {
		return InteractionResponse{}, errors.New("interaction result exceeds byte limit")
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: maxBytes + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return InteractionResponse{}, errors.New("interaction result must be a bounded JSON object")
	}
	id, ok := stringField(doc.Root(), "id")
	if !ok || id == "" || len(id) > 512 {
		return InteractionResponse{}, errors.New("interaction result has no bounded ID")
	}
	status, ok := stringField(doc.Root(), "status")
	if !ok || !validInteractionStatus(status) {
		return InteractionResponse{}, errors.New("interaction result has no valid status")
	}
	if status == "completed" {
		steps, present := doc.Root().Lookup("steps")
		if !present || steps.Kind() != oif.Array {
			return InteractionResponse{}, errors.New("completed interaction has no steps")
		}
	}
	return InteractionResponse{Source: doc, ID: id, Status: status}, nil
}

func (r InteractionResponse) WithID(id string) ([]byte, error) {
	return r.WithProjection(id, "", "")
}

func (r InteractionResponse) WithProjection(id, previous, route string) ([]byte, error) {
	changes := []oif.Change{{Pointer: "/id", Value: quoted(id), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini interaction"}}
	if route != "" {
		if _, ok := r.Source.Lookup("/model"); ok {
			changes = append(changes, oif.Change{Pointer: "/model", Value: quoted(route), Origin: oif.IdentityBinding, Reason: "published Gemini route"})
		}
	}
	if _, ok := r.Source.Lookup("/previous_interaction_id"); ok {
		if previous == "" {
			return nil, errors.New("provider response has an unbound previous interaction")
		}
		changes = append(changes, oif.Change{Pointer: "/previous_interaction_id", Value: quoted(previous), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini parent"})
	}
	doc, err := oif.Apply(r.Source, changes)
	if err != nil {
		return nil, err
	}
	return doc.Bytes(), nil
}

func validInteractionStatus(status string) bool {
	switch status {
	case "pending", "queued", "in_progress", "requires_action", "completed", "failed", "cancelled", "incomplete":
		return true
	}
	return false
}

type InteractionEvent struct {
	Source oif.Document
	Type   string
	ID     string
	Status string
}

func ParseInteractionEvent(data []byte, maxBytes int) (InteractionEvent, error) {
	if maxBytes < 1 || len(data) > maxBytes {
		return InteractionEvent{}, errors.New("interaction event exceeds byte limit")
	}
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxBytes + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return InteractionEvent{}, errors.New("interaction event must be a bounded JSON object")
	}
	kind, ok := stringField(doc.Root(), "event_type")
	if !ok {
		return InteractionEvent{}, errors.New("interaction event has no event_type")
	}
	switch kind {
	case "interaction.created", "interaction.status_update", "interaction.completed", "step.start", "step.delta", "step.stop", "error":
	default:
		return InteractionEvent{}, fmt.Errorf("unsupported Gemini interaction event type %q", kind)
	}
	event := InteractionEvent{Source: doc, Type: kind}
	if object, present := doc.Root().Lookup("interaction"); present && object.Kind() == oif.Object {
		event.ID, _ = stringField(object, "id")
		event.Status, _ = stringField(object, "status")
	}
	if rootID, hasRootID := stringField(doc.Root(), "interaction_id"); hasRootID {
		if event.ID != "" && event.ID != rootID {
			return InteractionEvent{}, errors.New("interaction event changed resource identity")
		}
		event.ID = rootID
	}
	if (kind == "interaction.created" || kind == "interaction.completed") && (event.ID == "" || len(event.ID) > 512) {
		return InteractionEvent{}, errors.New("interaction event has no bounded resource ID")
	}
	return event, nil
}

func (e InteractionEvent) WithID(id string) ([]byte, error) {
	return e.WithProjection(id, "", "")
}

func (e InteractionEvent) WithProjection(id, previous, route string) ([]byte, error) {
	changes := []oif.Change{}
	if _, ok := e.Source.Lookup("/interaction/id"); ok {
		changes = append(changes, oif.Change{Pointer: "/interaction/id", Value: quoted(id), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini interaction"})
	}
	if _, ok := e.Source.Lookup("/interaction_id"); ok {
		changes = append(changes, oif.Change{Pointer: "/interaction_id", Value: quoted(id), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini interaction"})
	}
	if _, ok := e.Source.Lookup("/interaction/previous_interaction_id"); ok {
		if previous == "" {
			return nil, errors.New("provider event has an unbound previous interaction")
		}
		changes = append(changes, oif.Change{Pointer: "/interaction/previous_interaction_id", Value: quoted(previous), Origin: oif.ResourceBinding, Reason: "owner-scoped Gemini parent"})
	}
	if route != "" {
		if _, ok := e.Source.Lookup("/interaction/model"); ok {
			changes = append(changes, oif.Change{Pointer: "/interaction/model", Value: quoted(route), Origin: oif.IdentityBinding, Reason: "published Gemini route"})
		}
	}
	if len(changes) == 0 {
		return e.Source.Bytes(), nil
	}
	doc, err := oif.Apply(e.Source, changes)
	if err != nil {
		return nil, err
	}
	return doc.Bytes(), nil
}

type InteractionStream struct {
	Created bool
	Step    bool
	Done    bool
	ID      string
	Resumed bool
	seen    bool
}

func (s *InteractionStream) Accept(event InteractionEvent) error {
	if s.Done {
		return errors.New("interaction event follows terminal event")
	}
	if s.Resumed && !s.seen {
		// A native cursor may resume in the middle of a step. It preserves the
		// provider event ID and resource affinity; no missing step is invented.
		if event.Type == "step.delta" || event.Type == "step.stop" {
			s.Step = true
		}
	}
	s.seen = true
	if !s.Created {
		if event.Type != "interaction.created" {
			return errors.New("interaction stream must begin with interaction.created")
		}
		s.Created, s.ID = true, event.ID
		return nil
	}
	if event.ID != "" && event.ID != s.ID {
		return errors.New("interaction stream changed resource identity")
	}
	switch event.Type {
	case "interaction.created":
		return errors.New("interaction stream created twice")
	case "step.start":
		if s.Step {
			return errors.New("interaction steps overlap")
		}
		s.Step = true
	case "step.delta":
		if !s.Step {
			return errors.New("interaction delta has no open step")
		}
	case "step.stop":
		if !s.Step {
			return errors.New("interaction stop has no open step")
		}
		s.Step = false
	case "interaction.completed", "error":
		if s.Step {
			return errors.New("interaction terminated during a step")
		}
		s.Done = true
	case "interaction.status_update":
	}
	return nil
}

type LiveSetup struct {
	Source oif.Document
	Model  string
}

func ParseLiveSetup(data []byte, maxBytes int) (LiveSetup, error) {
	if maxBytes < 1 || len(data) > maxBytes {
		return LiveSetup{}, errors.New("Gemini Live setup exceeds byte limit")
	}
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxBytes + 2048})
	if err != nil || doc.Root().Kind() != oif.Object || len(doc.Root().Members()) != 1 {
		return LiveSetup{}, errors.New("Gemini Live must begin with one setup object")
	}
	setup, ok := doc.Root().Lookup("setup")
	if !ok || setup.Kind() != oif.Object {
		return LiveSetup{}, errors.New("Gemini Live must begin with setup")
	}
	model, ok := stringField(setup, "model")
	if !ok || !strings.HasPrefix(model, "models/") || len(model) > 135 {
		return LiveSetup{}, errors.New("Gemini Live setup model must name a route")
	}
	for _, aliases := range [][]string{{"generationConfig", "generation_config"}, {"realtimeInputConfig", "realtime_input_config"}, {"sessionResumption", "session_resumption"}} {
		if _, a := setup.Lookup(aliases[0]); a {
			if _, b := setup.Lookup(aliases[1]); b {
				return LiveSetup{}, errors.New("Gemini Live setup has ambiguous field aliases")
			}
		}
	}
	for _, name := range []string{"sessionResumption", "session_resumption"} {
		if state, ok := setup.Lookup(name); ok && state.Kind() == oif.Object {
			if _, hasHandle := state.Lookup("handle"); hasHandle {
				return LiveSetup{}, errors.New("Gemini Live session resumption requires an authorized resource mapping")
			}
		}
	}
	return LiveSetup{Source: doc, Model: strings.TrimPrefix(model, "models/")}, nil
}

func (s LiveSetup) BindModel(model string) ([]byte, error) {
	doc, err := oif.Apply(s.Source, []oif.Change{{Pointer: "/setup/model", Value: quoted("models/" + strings.TrimPrefix(model, "models/")), Origin: oif.IdentityBinding, Reason: "qualified Gemini Live serving model"}})
	if err != nil {
		return nil, err
	}
	return doc.Bytes(), nil
}

func ValidateLiveClientFrame(data []byte, maxBytes int) error {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxBytes})
	if err != nil || doc.Root().Kind() != oif.Object || len(doc.Root().Members()) != 1 {
		return errors.New("Gemini Live frame must carry one client message")
	}
	for _, pair := range [][]string{{"clientContent", "client_content"}, {"realtimeInput", "realtime_input"}, {"toolResponse", "tool_response"}} {
		for _, name := range pair {
			if value, ok := doc.Root().Lookup(name); ok && value.Kind() == oif.Object {
				return nil
			}
		}
	}
	return errors.New("Gemini Live client frame has no supported message")
}

func ValidateLiveServerFrame(data []byte, maxBytes int) error {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: maxBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return errors.New("Gemini Live server frame must be a bounded JSON object")
	}
	principals := 0
	for _, member := range doc.Root().Members() {
		if member.Name == "usageMetadata" {
			if member.Value.Kind() != oif.Object {
				return errors.New("Gemini Live usage metadata must be an object")
			}
			continue
		}
		switch member.Name {
		case "setupComplete", "serverContent", "toolCall", "toolCallCancellation", "sessionResumptionUpdate", "goAway", "error":
			if member.Value.Kind() != oif.Object {
				return errors.New("Gemini Live principal event must be an object")
			}
			principals++
		default:
			return errors.New("Gemini Live server frame has an unregistered principal event")
		}
	}
	if principals != 1 {
		return errors.New("Gemini Live server frame must carry one principal event")
	}
	return nil
}

func stringField(parent oif.Value, name string) (string, bool) {
	v, ok := parent.Lookup(name)
	if !ok {
		return "", false
	}
	return v.Text()
}

func boolField(parent oif.Value, name string, fallback bool) (bool, error) {
	v, ok := parent.Lookup(name)
	if !ok {
		return fallback, nil
	}
	if v.Kind() != oif.Boolean {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return v.Raw() == "true", nil
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
