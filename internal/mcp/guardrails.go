package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/oif"
)

type toolGuardrailState struct {
	engine    *guardrails.Engine
	masks     *guardrails.Masking
	decisions []guardrails.Evidence
}

func guardrailRefusal(err error) *apiError {
	var refusal *guardrails.Error
	if errors.As(err, &refusal) {
		return fail(refusal.Status, refusal.Code, "invalid_request_error", "The tool call could not satisfy the configured guardrails.")
	}
	return fail(http.StatusInternalServerError, "guardrail_unavailable", "api_error", "The guardrail could not be evaluated.")
}

func (s *Server) toolGuardrails(in executeInput) *toolGuardrailState {
	if in.snapshot == nil {
		return nil
	}
	engine := in.snapshot.ToolGuardrailEngine(in.resolved.id, in.authority.ID)
	if engine == nil {
		return nil
	}
	return &toolGuardrailState{engine: engine, masks: &guardrails.Masking{}}
}

func stateDecisions(state *toolGuardrailState) []guardrails.Evidence {
	if state == nil {
		return nil
	}
	return slices.Clone(state.decisions)
}

func (s *Server) evaluateToolPhase(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState, phase string, segments []guardrails.Segment) (guardrails.Result, error) {
	if s.Guardrails == nil {
		return state.engine.Unavailable(phase)
	}
	return s.Guardrails(ctx, GuardrailEvaluation{
		Authority: in.authority, Release: in.release, Engine: state.engine,
		RequestID: reservation.AttemptID, Phase: phase, Segments: segments, Masks: state.masks,
	})
}

func (s *Server) refuseGuardedCall(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState) {
	now := s.now()
	evidence := AttemptEvidence{
		ServerID: in.tool.ServerID, ServerRevisionID: in.tool.ServerRevisionID,
		Status: StatusGuardrailBlocked, StartedAt: now, CompletedAt: now,
		GuardrailDecisions: stateDecisions(state),
	}
	if err := s.Ledger.RefuseUndispatched(context.WithoutCancel(ctx), reservation, evidence); err != nil {
		s.release(reservation)
	}
}

func unsupportedToolPhases(engine *guardrails.Engine, phases []string) ([]guardrails.Evidence, error) {
	if len(phases) == 0 {
		return nil, nil
	}
	result, err := engine.Unavailable(phases[0])
	if err != nil {
		return result.Decisions, err
	}
	next, failure := unsupportedToolPhases(engine, phases[1:])
	return slices.Concat(result.Decisions, next), failure
}

func (s *Server) guardArguments(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState) (json.RawMessage, *toolGuardrailState, *apiError) {
	if state == nil {
		return in.arguments, nil, nil
	}
	unavailable, failure := unsupportedToolPhases(state.engine, []string{guardrails.InputParallel, guardrails.OutputStream})
	if failure != nil {
		refused := &toolGuardrailState{engine: state.engine, masks: state.masks, decisions: unavailable}
		s.refuseGuardedCall(ctx, in, reservation, refused)
		return nil, refused, guardrailRefusal(failure)
	}
	document, parseErr := oif.ParseJSON(in.arguments, oif.Limits{MaxBytes: maxCallBodyBytes})
	if parseErr != nil {
		s.refuseGuardedCall(ctx, in, reservation, state)
		return nil, state, errGuardrailBlocked
	}
	surface := guardrails.ToolJSONSurface(document, in.tool.Alias, false)
	result, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Input, surface.Segments())
	guarded := &toolGuardrailState{engine: state.engine, masks: state.masks, decisions: slices.Concat(unavailable, result.Decisions)}
	if err != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, guardrailRefusal(err)
	}
	if !result.Changed {
		return in.arguments, guarded, nil
	}
	applied, applyErr := surface.Apply(result.Segments)
	if applyErr != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, errGuardrailBlocked
	}
	transformed := json.RawMessage(applied.Bytes())
	if len(transformed) > maxCallBodyBytes || in.tool.ValidateArguments(transformed) != nil {
		s.refuseGuardedCall(ctx, in, reservation, guarded)
		return nil, guarded, errGuardrailBlocked
	}
	return transformed, guarded, nil
}

type toolResultSurface struct {
	segments   []guardrails.Segment
	structured *guardrails.JSONSurface
	opaque     bool
}

func projectToolResult(in executeInput, result mcpservers.ToolResult) (toolResultSurface, error) {
	content := slices.Collect(func(yield func(guardrails.Segment) bool) {
		for index, block := range result.Content {
			if block.Type == "text" && !yield(guardrails.Segment{
				ID: "content" + strconv.Itoa(index), Kind: "tool_result",
				ToolName: in.tool.Alias, Untrusted: true, Text: block.Text,
			}) {
				return
			}
		}
	})
	opaque := slices.ContainsFunc(result.Content, func(block mcpservers.ToolContent) bool { return block.Type != "text" })
	if len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null" {
		return toolResultSurface{segments: content, opaque: opaque}, nil
	}
	document, err := oif.ParseJSON(result.StructuredContent, oif.Limits{MaxBytes: maxCallBodyBytes})
	if err != nil {
		return toolResultSurface{}, guardrails.ErrSurface
	}
	structured := guardrails.ToolJSONSurface(document, in.tool.Alias, true)
	return toolResultSurface{segments: slices.Concat(content, structured.Segments()), structured: structured, opaque: opaque}, nil
}

func applyToolResult(result mcpservers.ToolResult, surface toolResultSurface, segments []guardrails.Segment) (mcpservers.ToolResult, error) {
	texts := maps.Collect(func(yield func(string, string) bool) {
		for _, segment := range segments {
			if !yield(segment.ID, segment.Text) {
				return
			}
		}
	})
	for index, block := range result.Content {
		if block.Type != "text" {
			continue
		}
		_, ok := texts["content"+strconv.Itoa(index)]
		if !ok {
			return result, guardrails.ErrSurface
		}
	}
	content := slices.Collect(func(yield func(mcpservers.ToolContent) bool) {
		for index, block := range result.Content {
			if block.Type != "text" {
				if !yield(block) {
					return
				}
				continue
			}
			if !yield(mcpservers.ToolContent{Type: block.Type, Text: texts["content"+strconv.Itoa(index)], Data: block.Data, MimeType: block.MimeType}) {
				return
			}
		}
	})
	if surface.structured == nil {
		return mcpservers.ToolResult{Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
	}
	document, err := surface.structured.Apply(segments)
	if err != nil {
		return result, err
	}
	return mcpservers.ToolResult{Content: content, StructuredContent: document.Bytes(), IsError: result.IsError}, nil
}

func (s *Server) guardResult(ctx context.Context, in executeInput, reservation *Reservation, state *toolGuardrailState, result mcpservers.ToolResult) (mcpservers.ToolResult, []guardrails.Evidence, error) {
	if state == nil {
		return result, nil, nil
	}
	surface, projectErr := projectToolResult(in, result)
	if projectErr != nil {
		return result, stateDecisions(state), projectErr
	}
	if surface.opaque {
		unavailable, err := state.engine.Unavailable(guardrails.Output)
		return result, slices.Concat(state.decisions, unavailable.Decisions), err
	}
	segments := slices.Collect(func(yield func(guardrails.Segment) bool) {
		for _, segment := range surface.segments {
			if !yield(segment.Restore(state.masks)) {
				return
			}
		}
	})
	evaluated, err := s.evaluateToolPhase(ctx, in, reservation, state, guardrails.Output, segments)
	decisions := slices.Concat(state.decisions, evaluated.Decisions)
	if err != nil {
		return result, decisions, err
	}
	if !evaluated.Changed && slices.Equal(surface.segments, segments) {
		return result, decisions, nil
	}
	applied, err := applyToolResult(result, surface, evaluated.Segments)
	if err != nil {
		return result, decisions, err
	}
	encoded, _ := json.Marshal(applied)
	if len(encoded) > maxCallBodyBytes {
		return result, decisions, guardrails.ErrBlocked
	}
	return applied, decisions, nil
}
