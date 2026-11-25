package playground

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

func (p *API) Stream(w http.ResponseWriter, r *http.Request, userID string, admit Admit, in Request) error {
	if err := in.validate(true); err != nil {
		return err
	}
	run := p.member(r, userID, admit, in)
	parsed, err := in.parse("generation", "streaming", run)
	if err != nil {
		return err
	}
	sw := &streamWriter{w: w, maxTotal: p.kernel.Config.MaxResponseBytes}
	out := run.Run(r.Context(), parsed, sw.emit)
	if out.Cancelled {
		return nil
	}
	if out.Err != nil && !sw.committed {
		return fail(out.Err.Status, out.Err.Code, out.Err.Message)
	}
	sw.finish(run.Trail(), out)
	return nil
}

type streamWriter struct {
	w         http.ResponseWriter
	committed bool
	total     int64
	maxTotal  int64
}

func (sw *streamWriter) emit(frame []byte) error {
	payload, _ := json.Marshal(map[string]any{"data": string(frame)})
	return sw.write("frame", payload, true)
}

// write sends one SSE event. Only provider frames count against maxTotal: the
// terminal done or error event is gateway-generated and must still arrive after
// the frames have exhausted the budget, or the client sees a silent truncation.
func (sw *streamWriter) write(event string, data []byte, limited bool) error {
	chunk := int64(len(event) + len(data) + len("event: \ndata: \n\n"))
	if limited && sw.total+chunk > sw.maxTotal {
		return kernel.ErrResponseTooLarge
	}
	rc := http.NewResponseController(sw.w)
	if !sw.committed {
		// The playground answers a signed-in member, not an API key: there is no
		// allowance or key policy to state, so it adds no response headers.
		sw.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		sw.w.Header().Set("X-Accel-Buffering", "no")
		sw.w.WriteHeader(http.StatusOK)
		sw.committed = true
	}
	rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	sw.total += chunk
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	return nil
}

func (sw *streamWriter) finish(trail kernel.MemberTrail, out *kernel.Outcome) {
	if out.Err != nil {
		data, _ := json.Marshal(map[string]any{"code": out.Err.Code, "message": out.Err.Message, "status": out.Err.Status})
		_ = sw.write("error", data, false)
		return
	}
	meta := map[string]any{
		"id":      trail.ID,
		"model":   trail.Route,
		"routing": routingEvidence(trail),
	}
	if out.Completion != nil && out.Completion.Usage != nil {
		meta["usage"] = out.Completion.Usage
	}
	data, _ := json.Marshal(meta)
	_ = sw.write("done", data, false)
}
