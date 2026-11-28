package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/usage"
)

func upstreamString(body json.RawMessage, name string) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return "", false
	}
	raw, ok := obj[name]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return "", false
	}
	return value, true
}

// nativeResourceID proves one unambiguous native identity before a provider
// result may create or update its retained mapping. The source parser refuses
// duplicate (including escaped) keys and invalid Unicode spellings.
func NativeResourceID(body []byte, maxBytes int64) (string, bool) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(maxBytes)})
	if err != nil {
		return "", false
	}
	id, present := source.Root().Lookup("id")
	upstream, valid := id.Text()
	return upstream, present && valid && upstream != "" && len(upstream) <= 512
}

func RewriteID(body []byte, name, value string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("provider result is not a JSON object")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	obj[name] = encoded
	return json.Marshal(obj)
}

// rewritePrevious replaces an echoed upstream previous_response_id with its
// local id, or with null when no local id is known, so a provider identifier
// never reaches the caller.
func rewritePrevious(body []byte, local string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, present := obj["previous_response_id"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return body, nil
	}
	obj["previous_response_id"] = json.RawMessage("null")
	if local != "" {
		encoded, err := json.Marshal(local)
		if err != nil {
			return nil, err
		}
		obj["previous_response_id"] = encoded
	}
	return json.Marshal(obj)
}

func gateError(gate gateResult) *fault.Error {
	switch gate.verdict {
	case gateRejected:
		return gate.rejection.Fault()
	case gateExpired:
		return (&AttemptFailure{class: ClassTimeout}).Fault()
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "The pinned credential cannot currently serve this object.")
}

func (s *Server) WriteStateJSON(w http.ResponseWriter, x *Call, body []byte, exchanges ...*PinnedExchange) {
	s.WriteStateJSONStatus(w, x, body, http.StatusOK, exchanges...)
}

func (s *Server) WriteStateJSONStatus(w http.ResponseWriter, x *Call, body []byte, status int, exchanges ...*PinnedExchange) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if body != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(status)
	n, err := w.Write(body)
	if err == nil && n == len(body) {
		err = http.NewResponseController(w).Flush()
	}
	if err != nil || n != len(body) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The resource result could not be delivered.")
		for _, exchange := range exchanges {
			exchange.Delivery(DeliveryState(n))
		}
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	if n > 0 {
		x.Delivered(s.Now())
	}
	for _, exchange := range exchanges {
		exchange.Delivery(usage.ClientTerminal)
	}
	s.Finish(x, &Outcome{Committed: true}, status)
}

func ListArgs(r *http.Request) (int, string, *fault.Error) {
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxResourceList {
			param := "limit"
			return 0, "", fault.InvalidRequest("invalid_value", "limit must be an integer between 1 and 100.", &param)
		}
		limit = parsed
	}
	after := r.URL.Query().Get("after")
	if after != "" && !strings.Contains(after, "_") {
		param := "after"
		return 0, "", fault.InvalidRequest("invalid_value", "after must be a gateway object identifier.", &param)
	}
	return limit, after, nil
}

func (e *PinnedExchange) CompleteRetainedControl() {
	if e.request.retainedAdmission != nil {
		_ = ObserveRetainedUsage(e, oif.Document{}, false)
	}
	e.Complete(ClassSuccess)
}
