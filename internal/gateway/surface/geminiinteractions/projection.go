package geminiinteractions

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/usage"
)

func (t turn) readResult(response *kernel.PinnedResponse, invalid string) (geminilifecycle.InteractionResponse, *fault.Error) {
	limit := t.responseLimit()
	raw, err := response.ReadBody(limit)
	if err != nil {
		return geminilifecycle.InteractionResponse{}, fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The Interaction response exceeded its configured limit.")
	}
	result, err := geminilifecycle.ParseInteractionResponse(raw, int(limit))
	if err != nil {
		return geminilifecycle.InteractionResponse{}, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", invalid)
	}
	return result, nil
}

func (t turn) withoutCredentials(result geminilifecycle.InteractionResponse) (geminilifecycle.InteractionResponse, *fault.Error) {
	if result.Status != "failed" {
		return result, nil
	}
	source, err := fault.RedactNativeFailureDocument(result.Source, t.x.Sensitive())
	if err != nil {
		return geminilifecycle.InteractionResponse{}, fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Interaction failure could not be safely delivered.")
	}
	return geminilifecycle.InteractionResponse{Source: source, ID: result.ID, Status: result.Status}, nil
}

func (t turn) bounded(projection []byte) *fault.Error {
	if int64(len(projection)) > t.responseLimit() {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_too_large", "The projected Interaction exceeded its configured limit.")
	}
	return nil
}

func (t turn) deliver(response *kernel.PinnedResponse, projection []byte, undelivered string) {
	generation := !t.x.ResourceCall()
	t.w.Header().Set("Content-Type", "application/json")
	t.x.ResponseHeaders(t.w.Header(), false)
	if generation {
		t.w.Header().Set("X-Should-Retry", "false")
	}
	controller := http.NewResponseController(t.w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	t.w.WriteHeader(response.StatusCode)
	n, err := t.w.Write(projection)
	if err == nil && n == len(projection) {
		err = controller.Flush()
	}
	if err != nil || n != len(projection) {
		t.x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", undelivered)
		if generation {
			t.x.Failure.NoRetry = true
		}
		t.end.status, t.end.out = 0, &kernel.Outcome{Err: t.x.Failure, Committed: true, Cancelled: true}
		response.Exchange.Delivery(kernel.DeliveryState(n))
		return
	}
	t.x.Delivered(t.kernel.Now())
	t.end.status, t.end.out = response.StatusCode, &kernel.Outcome{Err: t.x.Failure, Committed: true}
	response.Exchange.Delivery(usage.ClientTerminal)
}

func (t turn) streamed(committed bool, err error, incomplete string) {
	if err == nil {
		t.end.status, t.end.out = http.StatusOK, &kernel.Outcome{Err: t.x.Failure, Committed: true}
		return
	}
	if errors.Is(err, fault.ErrClientWrite) || t.r.Context().Err() != nil {
		t.x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
		if !t.x.ResourceCall() {
			t.x.Failure.NoRetry = true
		}
		t.end.status, t.end.out = 0, &kernel.Outcome{Err: t.x.Failure, Committed: committed, Cancelled: true}
		return
	}
	t.x.Failure = fault.ServerError(http.StatusBadGateway, "interaction_incomplete", incomplete)
	t.end.status = t.x.Failure.Status
	if !committed {
		fault.WriteSurfaceError(t.w, t.x.Failure, "gemini")
	}
	t.end.out = &kernel.Outcome{Err: t.x.Failure, Committed: committed}
}

// Untouched OIF strings keep their source escape spelling. Inspect decoded
// values as well as member names before exposing an owner-scoped projection:
// a raw byte search misses IDs such as "int\\u005ffoo".
func containsNativeResourceID(body []byte, ids ...string) bool {
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: len(body)})
	if err != nil {
		return true
	}
	contains := func(value string) bool {
		for _, id := range ids {
			if id != "" && strings.Contains(value, id) {
				return true
			}
		}
		return false
	}
	var visit func(oif.Value) bool
	visit = func(value oif.Value) bool {
		switch value.Kind() {
		case oif.String:
			text, ok := value.Text()
			return !ok || contains(text)
		case oif.Object:
			for _, member := range value.Members() {
				if contains(member.Name) || visit(member.Value) {
					return true
				}
			}
		case oif.Array:
			for _, item := range value.Elements() {
				if visit(item) {
					return true
				}
			}
		}
		return false
	}
	return visit(doc.Root())
}

func generationFailure() *fault.Error {
	failure := fault.ServerError(http.StatusBadGateway, "upstream_response_failed", "The provider reported a failed Interaction generation.")
	failure.NoRetry = true
	return failure
}

func unsupportedStatus() *fault.Error {
	return fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The provider returned an unsupported Interaction status.")
}
