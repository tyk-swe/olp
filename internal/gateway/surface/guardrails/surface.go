package guardrails

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/guardrails"
)

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
	if err := ingress.DecodeStrict(r, &in, guardrails.MaxContentBytes); err != nil {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Supply a policy revision, phase and bounded text segments.", nil))
		return
	}
	applied, e := s.kernel.ApplyGuardrails(r.Context(), w.Header(), x, in)
	s.reply(w, applied, e)
	s.kernel.SettleAdmission(context.WithoutCancel(r.Context()), x)
	s.kernel.Finish(x, &kernel.Outcome{Err: e}, status(e))
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
