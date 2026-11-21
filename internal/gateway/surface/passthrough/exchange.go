package passthrough

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

type exchangeEnd struct{ pin *kernel.Pin }

func (s surface) exchange(w http.ResponseWriter, r *http.Request, x *kernel.Call, path string, query url.Values) {
	pin, e := s.kernel.SelectPassthrough(r.Context(), x)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout(x.Route, pin.Target))
	defer cancel()
	deadline, _ := ctx.Deadline()
	_ = http.NewResponseController(w).SetWriteDeadline(deadline)
	body, e := s.readBody(ctx, w, r, x.Route, deadline)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	upstream, e := s.upstreamURL(pin.Provider, path, query)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	end := &exchangeEnd{}
	defer func() { s.kernel.SettlePassthrough(r.Context(), x, end.pin) }()
	gated, e := s.kernel.GatePassthrough(ctx, x, pin, deadline)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	end.pin = &gated
	resp, e := s.kernel.DispatchPassthrough(ctx, x, gated, r, upstream, body)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	defer resp.Body.Close()
	source, limit, e := s.kernel.ReceivePassthrough(ctx, x, r.Method, resp)
	if e != nil {
		s.kernel.RefusePassthrough(x, w, e)
		return
	}
	writeHeader(w, r.Method, resp, x.Sensitive())
	x.Delivered(s.kernel.Now())
	readErr, copyErr := copyResponse(w, source, limit)
	s.kernel.FinishPassthrough(ctx, x, resp.StatusCode, readErr, copyErr)
	if readErr != nil || copyErr != nil {
		panic(http.ErrAbortHandler)
	}
}

func timeout(route *routing.Route, target routing.Target) time.Duration {
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if target.Timeout > 0 {
		return min(overall, time.Duration(target.Timeout)*time.Millisecond)
	}
	return overall
}

func (s surface) bodyLimit(route *routing.Route) int64 {
	limit := s.kernel.Config.MaxBodyBytes
	if route.MaxBodyBytes != nil {
		limit = min(limit, *route.MaxBodyBytes)
	}
	if limit <= 0 {
		return 64 * 1024
	}
	return limit
}

func (s surface) readBody(ctx context.Context, w http.ResponseWriter, r *http.Request, route *routing.Route, deadline time.Time) ([]byte, *fault.Error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	limit := s.bodyLimit(route)
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request body could not be bounded.")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, kernel.ClassFailure(kernel.ClassTimeout)
		}
		return nil, fault.InvalidRequest("invalid_request", "The request body could not be read.", nil)
	}
	_ = controller.SetReadDeadline(time.Time{})
	if int64(len(body)) > limit {
		return nil, ingress.BodyTooLarge()
	}
	return body, nil
}

func (s surface) upstreamURL(provider routing.Provider, path string, query url.Values) (string, *fault.Error) {
	base, err := s.kernel.Egress.ValidateEndpoint(provider.Endpoint)
	if err != nil {
		return "", fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "The bound connection endpoint is unavailable.")
	}
	upstream := *base
	upstream.Path = strings.TrimSuffix(base.Path, "/") + path
	upstream.RawPath = ""
	upstream.RawQuery = query.Encode()
	upstream.Fragment = ""
	return upstream.String(), nil
}

func writeHeader(w http.ResponseWriter, method string, resp *http.Response, sensitive egress.Sensitive) {
	for name, values := range codewire.ForwardHeaders(resp.Header, false) {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if routing.HopByHopOrCredentialHeader(canonical) || sensitive.Contains(canonical) {
			continue
		}
		if method != http.MethodHead && resp.StatusCode >= 400 && (canonical == "Content-Encoding" || canonical == "Content-Md5" || canonical == "Digest") {
			continue
		}
		for _, value := range values {
			w.Header().Add(canonical, sensitive.Redact(value))
		}
	}
	if method == http.MethodHead && resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.WriteHeader(resp.StatusCode)
}

func copyResponse(w http.ResponseWriter, source io.Reader, limit int64) (error, error) {
	bounded := &io.LimitedReader{R: source, N: limit + 1}
	buffer := make([]byte, 32<<10)
	controller := http.NewResponseController(w)
	var deliveryErr error
	for {
		n, readErr := bounded.Read(buffer)
		if bounded.N == 0 {
			return io.ErrShortBuffer, deliveryErr
		}
		if n > 0 && deliveryErr == nil {
			deliveryErr = deliver(w, controller, buffer[:n])
		}
		if errors.Is(readErr, io.EOF) {
			return nil, deliveryErr
		}
		if readErr != nil {
			return readErr, deliveryErr
		}
	}
}

func deliver(w http.ResponseWriter, controller *http.ResponseController, chunk []byte) error {
	written, err := w.Write(chunk)
	switch {
	case err != nil:
		return err
	case written != len(chunk):
		return io.ErrShortWrite
	default:
		return controller.Flush()
	}
}
