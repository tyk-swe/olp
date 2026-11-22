package realtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

type webrtcCallResult struct {
	Answer   []byte
	Status   int
	CallID   string
	Conn     *websocket.Conn
	Rejected bool
	Exposure bool
}

func parseWebRTCCallID(location string) (string, bool) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Path == "" {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return "", false
	}
	return segments[len(segments)-1], true
}

func (s surface) dialWebRTCControl(ctx context.Context, x *kernel.Call, p *kernel.Pin, endpoint string) (*websocket.Conn, *fault.Error) {
	limit := s.kernel.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	return s.kernel.DialPinnedWebSocket(ctx, x, p, endpoint, bodylimit.Lower(limit, x.Route.MaxBodyBytes))
}

func (s surface) hangupWebRTCCall(parent context.Context, x *kernel.Call, p *kernel.Pin, callID string, closeTimeout time.Duration) error {
	if callID == "" {
		return errors.New("no provider call identity")
	}
	endpoint, err := p.Provider.Connector().RealtimeHangupURL(p.Model, callID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), closeTimeout)
	defer cancel()
	status, err := s.kernel.DoPinnedAuxPost(ctx, x, p, endpoint)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return fmt.Errorf("provider hangup status %d", status)
	}
	return nil
}

func (s surface) createWebRTCCall(ctx context.Context, x *kernel.Call, p *kernel.Pin, body []byte, contentType string, closeTimeout time.Duration, maxAnswerBytes int64) (webrtcCallResult, *fault.Error) {
	cfg := p.Provider.Connector()
	endpoint, err := cfg.RealtimeCallsURL(p.Model)
	if err != nil {
		return webrtcCallResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	if _, err := s.kernel.Egress.ValidateEndpoint(endpoint); err != nil {
		return webrtcCallResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy.")
	}
	createCtx := ctx
	cancelCreate := context.CancelFunc(func() {})
	if p.Timeout() > 0 {
		createCtx, cancelCreate = context.WithTimeout(ctx, p.Timeout())
	}
	defer cancelCreate()
	sent, e := s.kernel.BeginSessionCreate(ctx, createCtx, x, p, "webrtc", endpoint, contentType, "application/sdp", body)
	if e != nil {
		return webrtcCallResult{Rejected: sent.Rejected, Exposure: sent.Exposure}, e
	}
	defer sent.Response.Body.Close()
	result := webrtcCallResult{Rejected: sent.Rejected, Exposure: sent.Exposure}
	callID, ok := parseWebRTCCallID(sent.Response.Header.Get("Location"))
	answer, readErr := io.ReadAll(io.LimitReader(sent.Response.Body, maxAnswerBytes+1))
	mediaType, _, mediaErr := mime.ParseMediaType(sent.Response.Header.Get("Content-Type"))
	if readErr != nil || len(answer) == 0 || int64(len(answer)) > maxAnswerBytes || mediaErr != nil || mediaType != "application/sdp" {
		sent.Attempt.Fail(kernel.ClassProtocol)
		if !ok || s.hangupWebRTCCall(ctx, x, p, callID, closeTimeout) != nil {
			result.Exposure = true
		}
		return result, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an unusable session answer.")
	}
	controlEndpoint, controlErr := cfg.RealtimeControlURL(p.Model, callID)
	if !ok || controlErr != nil {
		sent.Attempt.Fail(kernel.ClassProtocol)
		result.Exposure = true
		return result, fault.ServerError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	sent.Attempt.Accept()
	conn, dialErr := s.dialWebRTCControl(createCtx, x, p, controlEndpoint)
	if dialErr != nil {
		if s.hangupWebRTCCall(ctx, x, p, callID, closeTimeout) != nil {
			result.Exposure = true
		}
		return result, dialErr
	}
	result.Answer, result.Status, result.CallID, result.Conn = answer, sent.Response.StatusCode, callID, conn
	return result, nil
}
