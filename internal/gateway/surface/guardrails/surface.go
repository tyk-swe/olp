package guardrails

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/guardrails"
)

var errNotJSON = errors.New("request body is not application/json")

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/guardrails/apply", surface{kernel: k}.apply)
}

func (s surface) apply(w http.ResponseWriter, r *http.Request) {
	x, ok := s.kernel.EnterGuardrails(w, r)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	var in guardrails.Application
	if err := decodeStrict(r, &in, guardrails.MaxContentBytes); err != nil {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Supply a policy revision, phase and bounded text segments.", nil))
		return
	}
	applied, e := s.kernel.ApplyGuardrails(r.Context(), w.Header(), x, in)
	s.reply(w, applied, e)
	s.kernel.SettleAdmission(context.WithoutCancel(r.Context()), x)
	s.kernel.Finish(x, &kernel.Outcome{Err: e}, status(e))
}

func decodeStrict(r *http.Request, destination any, limit int64) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errNotJSON
	}
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	return oif.DecodeStrict(data, destination, oif.StrictLimits(limit))
}

func (s surface) reply(w http.ResponseWriter, applied guardrails.AppliedResult, e *fault.Error) {
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	fault.WriteJSON(w, applied)
}

func status(e *fault.Error) int {
	if e != nil {
		return e.Status
	}
	return http.StatusOK
}
