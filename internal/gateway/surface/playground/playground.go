package playground

import (
	"cmp"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

const Timeout = 2 * time.Minute

const BodyLimit = 1 << 20

type Admit func(*routing.Route) *fault.Error

type Error struct {
	Status  int
	Code    string
	Message string
	Field   string
}

func (e *Error) Error() string { return e.Message }

func fail(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}

func invalid(field, message string) error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: "validation_failed", Message: message, Field: field}
}

type Result struct {
	Body                any
	DisableHTMLEscaping bool
}

type API struct {
	kernel *kernel.Server
}

func NewAPI(k *kernel.Server) *API { return &API{kernel: k} }

func (p *API) member(r *http.Request, userID string, admit Admit, in Request) *kernel.MemberGeneration {
	return p.kernel.MemberGeneration(r, kernel.Member{UserID: userID, Preferences: in.Routing, Document: in.Request, Admit: admit})
}

func (p *API) Handle(r *http.Request, userID string, admit Admit, in Request) (Result, error) {
	if err := in.validate(false); err != nil {
		return Result{}, err
	}
	run := p.member(r, userID, admit, in)
	parsed, err := in.parse(in.operation(), "unary", run)
	if err != nil {
		return Result{}, err
	}
	out := run.Run(r.Context(), parsed, nil)
	if e := out.Err; e != nil {
		return Result{}, fail(cmp.Or(e.Status, http.StatusBadGateway), e.Code, e.Message)
	}
	return Result{Body: response(run.Trail(), out.Completion, in), DisableHTMLEscaping: in.operation() != "generation"}, nil
}
