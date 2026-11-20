package realtime

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/platform/panics"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
)

func addUsageShare(dst **int64, share *int64) {
	if share == nil {
		return
	}
	total := *share
	if *dst != nil {
		total = quota.AddBounded(total, **dst)
	}
	*dst = &total
}

func (sess *webrtcSession) accumulateUsage(u *wire.Usage) {
	sess.usage.InputTokens = quota.AddBounded(sess.usage.InputTokens, u.InputTokens)
	sess.usage.OutputTokens = quota.AddBounded(sess.usage.OutputTokens, u.OutputTokens)
	addUsageShare(&sess.usage.CachedInputTokens, u.CachedInputTokens)
	addUsageShare(&sess.usage.AudioInputTokens, u.AudioInputTokens)
	addUsageShare(&sess.usage.CachedAudioInputTokens, u.CachedAudioInputTokens)
	addUsageShare(&sess.usage.AudioOutputTokens, u.AudioOutputTokens)
	addUsageShare(&sess.usage.ImageInputTokens, u.ImageInputTokens)
	addUsageShare(&sess.usage.CachedImageInputTokens, u.CachedImageInputTokens)
	sess.haveUsage = true
}

func (s surface) runWebRTCControl(sess *webrtcSession) {
	readErr := make(chan error, 1)
	go func() {
		defer panics.Recover(s.kernel.Log, "webrtc control reader", func(err error) { readErr <- err })
		for {
			typ, data, err := sess.conn.Read(sess.ctx)
			if err != nil {
				readErr <- err
				return
			}
			func() {
				sess.usageMu.Lock()
				defer sess.usageMu.Unlock()
				if typ == websocket.MessageText && bytes.Contains(data, []byte("input_audio_transcription")) {
					sess.responses.Untrackable()
				}
				if u := sess.responses.ProviderFrame(typ, data, true); u != nil {
					sess.accumulateUsage(u)
				}
			}()
		}
	}()
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	for {
		select {
		case err := <-readErr:
			s.finishWebRTCControl(sess, err)
			return
		case <-reauth.C:
			if e := s.webrtcAuthority(sess); e != nil {
				if err := s.kernel.HangupWebRTCCall(sess.ctx, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
					s.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
				}
				s.settleWebRTC(sess, e)
				sess.cancel()
				return
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(sess.ctx, ingress.ReauthInterval)
			err := sess.conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				s.finishWebRTCControl(sess, err)
				return
			}
		case <-sess.ctx.Done():
			if err := s.kernel.HangupWebRTCCall(sess.ctx, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
				s.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
			}
			s.settleWebRTC(sess, fault.ServerError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."))
			return
		}
	}
}

func (s surface) webrtcAuthority(sess *webrtcSession) *fault.Error {
	return s.kernel.SessionAuthority(sess.ctx, sess.x, sess.token, sess.keyID, sess.p)
}

func (s surface) finishWebRTCControl(sess *webrtcSession, readErr error) {
	code := websocket.CloseStatus(readErr)
	sess.usageMu.Lock()
	pending := sess.responses.Pending()
	sess.usageMu.Unlock()
	if (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) && !pending && sess.ctx.Err() == nil {
		if err := s.kernel.HangupWebRTCCall(sess.ctx, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
			s.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
			s.settleWebRTC(sess, fault.ServerError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."))
			sess.cancel()
			return
		}
		s.settleWebRTC(sess, nil)
		sess.cancel()
		return
	}
	if errors.Is(sess.ctx.Err(), context.DeadlineExceeded) {
		if err := s.kernel.HangupWebRTCCall(sess.ctx, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
			s.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
		}
		s.settleWebRTC(sess, fault.ServerError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."))
		return
	}
	if err := s.kernel.HangupWebRTCCall(sess.ctx, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
		s.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
	}
	s.settleWebRTC(sess, fault.ServerError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."))
	sess.cancel()
}

func (s surface) settleWebRTC(sess *webrtcSession, failure *fault.Error) {
	sess.settleOnce.Do(func() {
		defer sess.admission.Release()
		s.kernel.BeginWebRTCSettlement(sess.ctx, sess.x, sess.rejected, failure)
		if sess.conn != nil {
			_ = sess.conn.CloseNow()
		}
		sess.usageMu.Lock()
		var observed *wire.Usage
		if sess.haveUsage {
			usageCopy := sess.usage
			observed = &usageCopy
		}
		pending := sess.responses.Pending()
		sess.usageMu.Unlock()
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(sess.ctx), webrtcCloseTimeout)
		s.kernel.FinishWebRTCSettlement(settleCtx, sess.x, sess.p, observed, pending, sess.rejected, failure)
		cancel()
		s.calls.unregister(sess.id)
		sess.cancel()
	})
}
