package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func addUsageShare(dst **int64, share *int64) {
	if share == nil {
		return
	}
	total := *share
	if *dst != nil {
		total = addBounded(total, **dst)
	}
	*dst = &total
}

func (sess *webrtcSession) accumulateUsage(u *wire.Usage) {
	sess.usage.InputTokens = addBounded(sess.usage.InputTokens, u.InputTokens)
	sess.usage.OutputTokens = addBounded(sess.usage.OutputTokens, u.OutputTokens)
	addUsageShare(&sess.usage.CachedInputTokens, u.CachedInputTokens)
	addUsageShare(&sess.usage.AudioInputTokens, u.AudioInputTokens)
	addUsageShare(&sess.usage.CachedAudioInputTokens, u.CachedAudioInputTokens)
	addUsageShare(&sess.usage.AudioOutputTokens, u.AudioOutputTokens)
	addUsageShare(&sess.usage.ImageInputTokens, u.ImageInputTokens)
	addUsageShare(&sess.usage.CachedImageInputTokens, u.CachedImageInputTokens)
	sess.haveUsage = true
}

func (s *Server) runWebRTCControl(sess *webrtcSession) {
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := sess.conn.Read(sess.ctx)
			if err != nil {
				readErr <- err
				return
			}
			sess.usageMu.Lock()
			if typ == websocket.MessageText && bytes.Contains(data, []byte("input_audio_transcription")) {
				sess.responses.unknown = true
			}
			if u := sess.responses.providerFrame(typ, data, true); u != nil {
				sess.accumulateUsage(u)
			}
			sess.usageMu.Unlock()
		}
	}()
	reauth := time.NewTicker(s.cfg.WebRTCReauth)
	defer reauth.Stop()
	heartbeat := time.NewTicker(s.cfg.WebRTCHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case err := <-readErr:
			s.finishWebRTCControl(sess, err)
			return
		case <-reauth.C:
			if e := s.webrtcAuthority(sess); e != nil {
				if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
					s.holdWebRTCExposure(sess.keyID, sess.started.Add(s.cfg.WebRTCBound))
				}
				s.settleWebRTC(sess, e)
				sess.cancel()
				return
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(sess.ctx, s.cfg.WebRTCReauth)
			err := sess.conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				s.finishWebRTCControl(sess, err)
				return
			}
		case <-sess.ctx.Done():
			if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
				s.holdWebRTCExposure(sess.keyID, sess.started.Add(s.cfg.WebRTCBound))
			}
			s.settleWebRTC(sess, serverError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."))
			return
		}
	}
}

func (s *Server) webrtcAuthority(sess *webrtcSession) *Error {
	x := sess.x
	authority, err := s.reauthenticateSession(sess.ctx, x, sess.token)
	if err != nil || authority == nil || authority.ID != sess.keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return permissionError("key_revoked", "The realtime session no longer has route authority.")
	}
	if eligibility := s.pinEligibility(sess.p); eligibility != routing.Eligible {
		return permissionError("provider_credential_"+string(eligibility), "The realtime provider credential is no longer eligible.")
	}
	current := *authority
	current.EndUserDigest = x.endUserDigest
	if e := s.unboundedPinLimits(x.admissionAuthority(current), sess.p, x.route); e != nil {
		return e
	}
	return nil
}

func (s *Server) finishWebRTCControl(sess *webrtcSession, readErr error) {
	code := websocket.CloseStatus(readErr)
	sess.usageMu.Lock()
	pending := sess.responses.pending()
	sess.usageMu.Unlock()
	if (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) && !pending && sess.ctx.Err() == nil {
		if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
			s.holdWebRTCExposure(sess.keyID, sess.started.Add(s.cfg.WebRTCBound))
			s.settleWebRTC(sess, serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."))
			sess.cancel()
			return
		}
		s.settleWebRTC(sess, nil)
		sess.cancel()
		return
	}
	if errors.Is(sess.ctx.Err(), context.DeadlineExceeded) {
		if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
			s.holdWebRTCExposure(sess.keyID, sess.started.Add(s.cfg.WebRTCBound))
		}
		s.settleWebRTC(sess, serverError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."))
		return
	}
	if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
		s.holdWebRTCExposure(sess.keyID, sess.started.Add(s.cfg.WebRTCBound))
	}
	s.settleWebRTC(sess, serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."))
	sess.cancel()
}

func (s *Server) settleWebRTC(sess *webrtcSession, failure *Error) {
	sess.settleOnce.Do(func() {
		defer sess.admission.Release()
		x := sess.x
		s.finishSessionDebit(sess.ctx, x, failure != nil && !sess.rejected)
		if sess.conn != nil {
			_ = sess.conn.CloseNow()
		}
		sess.usageMu.Lock()
		var observed *wire.Usage
		if sess.haveUsage {
			usageCopy := sess.usage
			observed = &usageCopy
		}
		pending := sess.responses.pending()
		sess.usageMu.Unlock()
		class := classProtocol
		cancelled := false
		if failure != nil {
			switch {
			case failure.Code == "key_revoked" || strings.HasPrefix(failure.Code, "provider_credential_"):
				class = classCredential
			case failure.Code == "realtime_session_expired":
				class = classTimeout
			}
		}
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			if fact.Interaction != nil {
				if observed != nil {
					fact.Interaction.ClientState = usage.ClientPartial
				}
				if failure == nil || sess.rejected {
					fact.Interaction.UpstreamState = usage.UpstreamTerminal
					fact.Interaction.ClientState = usage.ClientTerminal
				} else {
					fact.Interaction.UpstreamState = usage.UpstreamUnknown
				}
			}
			if observed != nil {
				fact.Usage = observed
				fact.UsageObserved = true
				fact.UsageComplete = failure == nil && !pending
				fact.BillingUncertain = !fact.UsageComplete
			} else if x.dispatched && !sess.rejected {
				fact.UsageComplete = false
				fact.BillingUncertain = true
			}
			if failure != nil && fact.Class == "success" {
				fact.Class = class
			}
		}
		status := http.StatusOK
		var out *outcome
		if failure != nil {
			x.failure = failure
			status = failure.Status
			out = &outcome{err: failure, committed: x.dispatched, cancelled: cancelled}
		} else {
			out = &outcome{committed: true}
		}
		s.finish(x, out, status)
		settled := geminiLiveSettlement(x)
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(sess.ctx), webrtcCloseTimeout)
		s.settlePinHold(settleCtx, x, sess.p.hold, settled)
		s.recordCost(x)
		settleKey(settleCtx, x.lease, x.dispatched, settled, s.log)
		s.settleCaps(settleCtx, x)
		cancel()
		s.unregisterWebRTCSession(sess.id)
		sess.cancel()
	})
}
