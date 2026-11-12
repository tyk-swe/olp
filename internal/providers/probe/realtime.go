package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/duplexcontract"
)

func (p *Prober) certifyRealtime(ctx context.Context, t Target, model string) error {
	endpoint, err := t.Connection.Transport().RealtimeURL(model)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The selected profile cannot address this realtime model."}
	}
	return p.certifyWebSocketDial(ctx, t, endpoint, nil)
}

func (p *Prober) certifyNativeDuplex(ctx context.Context, t Target, model string) error {
	transport := t.Connection.Transport()
	template, err := duplexcontract.Compile(duplexcontract.Config{Provider: transport, Model: model})
	if err != nil {
		return &Failure{Reason: CapabilityUnavailable, Detail: "The native duplex contract could not be compiled."}
	}
	switch template.Transport() {
	case duplexcontract.TransportWebSocket:
		endpoint, err := transport.DuplexURL(model)
		if err != nil {
			return &Failure{Reason: InvalidEndpoint, Detail: "The selected profile cannot address this realtime model."}
		}
		return p.certifyWebSocketDial(ctx, t, endpoint, func(ctx context.Context, conn *websocket.Conn) error {
			return probeNativeDuplexSetup(ctx, conn, template, transport, model)
		})
	case duplexcontract.TransportBedrockBidi:
		endpoint, err := transport.BedrockDuplexEndpoint()
		if err != nil {
			return &Failure{Reason: InvalidEndpoint, Detail: "The selected profile cannot address this realtime model."}
		}
		if _, err := p.egress.ValidateEndpoint(endpoint); err != nil {
			return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
		}
		ctx, release, err := p.slot(ctx)
		if err != nil {
			return err
		}
		defer release()
		client, err := p.Client(ctx, t)
		if err != nil {
			return &Failure{Reason: NetworkCredentialInvalid, Detail: "The configured provider network connection is unavailable."}
		}
		stream, _, err := p.auth.OpenBedrockDuplex(ctx, transport, t.Credential, model, client)
		if err != nil {
			return Classify(err)
		}
		_ = stream.Close()
		return nil
	}
	return &Failure{Reason: CapabilityUnavailable, Detail: "The native duplex transport cannot be certified."}
}

func (p *Prober) certifyWebSocketDial(ctx context.Context, t Target, endpoint string, exchange func(context.Context, *websocket.Conn) error) error {
	transport := t.Connection.Transport()
	check := strings.Replace(endpoint, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	u, err := url.Parse(check)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	u.RawQuery = ""
	if _, err := p.egress.ValidateEndpoint(u.String()); err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	ctx, release, err := p.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	if _, err := p.auth.Apply(ctx, req, transport, t.Credential, nil); err != nil {
		return &Failure{Reason: CredentialInvalid, Detail: err.Error()}
	}
	client, err := p.Client(ctx, t)
	if err != nil {
		return &Failure{Reason: NetworkCredentialInvalid, Detail: "The configured provider network connection is unavailable."}
	}
	conn, resp, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: req.Header})
	if err != nil {
		if resp != nil && resp.StatusCode != 0 {
			return statusError(t, resp.StatusCode, nil)
		}
		return Classify(err)
	}
	defer conn.CloseNow()
	if exchange != nil {
		if err := exchange(ctx, conn); err != nil {
			return err
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func probeNativeDuplexSetup(ctx context.Context, conn *websocket.Conn, template *duplexcontract.Template, transport connectors.Config, model string) error {
	if !template.SetupAck() {
		return nil
	}
	boundModel, err := transport.DuplexBoundModel(model)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The selected profile cannot address this realtime model."}
	}
	setup, _ := json.Marshal(map[string]map[string]string{"setup": {"model": boundModel}})
	conn.SetReadLimit(BodyLimit)
	if err := conn.Write(ctx, websocket.MessageText, setup); err != nil {
		return Classify(err)
	}
	kind, raw, err := conn.Read(ctx)
	if err != nil {
		return Classify(err)
	}
	if kind != websocket.MessageText {
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not acknowledge the model setup."}
	}
	frame, err := template.Observer(BodyLimit).ServerFrame(raw)
	if err != nil || !frame.SetupComplete {
		return &Failure{Reason: ProviderProtocolError, Detail: "The upstream did not acknowledge the model setup."}
	}
	return nil
}

const webrtcProbeSDP = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"a=group:BUNDLE 0 1\r\n" +
	"a=extmap-allow-mixed\r\n" +
	"a=msid-semantic: WMS olp-probe\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=rtcp:9 IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:olpp\r\n" +
	"a=ice-pwd:olp-probe-password-0000000000\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:0\r\n" +
	"a=sendrecv\r\n" +
	"a=rtcp-mux\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=ice-ufrag:olpp\r\n" +
	"a=ice-pwd:olp-probe-password-0000000000\r\n" +
	"a=ice-options:trickle\r\n" +
	"a=fingerprint:sha-256 00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00:00\r\n" +
	"a=setup:actpass\r\n" +
	"a=mid:1\r\n" +
	"a=sctp-port:5000\r\n"

func (p *Prober) certifyWebRTC(ctx context.Context, t Target, model string) error {
	transport := t.Connection.Transport()
	endpoint, err := transport.RealtimeCallsURL(model)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: "The selected profile cannot address this realtime model."}
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	u.RawQuery = ""
	if _, err := p.egress.ValidateEndpoint(u.String()); err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	ctx, release, err := p.slot(ctx)
	if err != nil {
		return err
	}
	defer release()
	session, _ := json.Marshal(map[string]string{"type": "realtime", "model": transport.Model(model)})
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	sdpPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="sdp"`},
		"Content-Type":        {"application/sdp"},
	})
	if err != nil {
		return &Failure{Reason: InvalidRequest, Detail: err.Error()}
	}
	if _, err := io.WriteString(sdpPart, webrtcProbeSDP); err != nil {
		return &Failure{Reason: InvalidRequest, Detail: err.Error()}
	}
	sessionPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="session"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return &Failure{Reason: InvalidRequest, Detail: err.Error()}
	}
	if _, err := sessionPart.Write(session); err != nil {
		return &Failure{Reason: InvalidRequest, Detail: err.Error()}
	}
	if err := writer.Close(); err != nil {
		return &Failure{Reason: InvalidRequest, Detail: err.Error()}
	}
	body := form.Bytes()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/sdp")
	if _, err := p.auth.Apply(ctx, req, transport, t.Credential, body); err != nil {
		return &Failure{Reason: CredentialInvalid, Detail: err.Error()}
	}
	client, err := p.Client(ctx, t)
	if err != nil {
		return &Failure{Reason: NetworkCredentialInvalid, Detail: "The configured provider network connection is unavailable."}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Classify(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, BodyLimit))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(t, resp.StatusCode, nil)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || location.Path == "" {
		return &Failure{Reason: ControlUnavailable, Detail: "The provider returned no controllable session identity."}
	}
	segments := strings.Split(strings.Trim(location.Path, "/"), "/")
	callID := segments[len(segments)-1]
	controlErr := p.certifyWebRTCControl(ctx, t, model, callID, client)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), probeCancelWait)
	defer cleanupCancel()
	hangupErr := p.hangupWebRTCProbe(cleanupCtx, t, model, callID, client)
	return errors.Join(controlErr, hangupErr)
}

func (p *Prober) certifyWebRTCControl(ctx context.Context, t Target, model, callID string, client *http.Client) error {
	control, err := t.Connection.Transport().RealtimeControlURL(model, callID)
	if err != nil {
		return &Failure{Reason: ControlUnavailable, Detail: "The provider returned no controllable session identity."}
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, control, nil)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	if _, err := p.auth.Apply(ctx, probe, t.Connection.Transport(), t.Credential, nil); err != nil {
		return &Failure{Reason: CredentialInvalid, Detail: err.Error()}
	}
	conn, dialResp, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: probe.Header})
	if err != nil {
		if dialResp != nil && dialResp.StatusCode != 0 {
			return statusError(t, dialResp.StatusCode, nil)
		}
		return Classify(err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func (p *Prober) hangupWebRTCProbe(ctx context.Context, t Target, model, callID string, client *http.Client) error {
	hangupURL, err := t.Connection.Transport().RealtimeHangupURL(model, callID)
	if err != nil {
		return &Failure{Reason: ControlUnavailable, Detail: "The provider returned no controllable session identity."}
	}
	hangup, err := http.NewRequestWithContext(ctx, http.MethodPost, hangupURL, nil)
	if err != nil {
		return &Failure{Reason: InvalidEndpoint, Detail: err.Error()}
	}
	if _, err := p.auth.Apply(ctx, hangup, t.Connection.Transport(), t.Credential, nil); err != nil {
		return &Failure{Reason: CredentialInvalid, Detail: err.Error()}
	}
	hangupResp, err := client.Do(hangup)
	if err != nil {
		return Classify(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(hangupResp.Body, BodyLimit))
	hangupResp.Body.Close()
	if hangupResp.StatusCode < 200 || hangupResp.StatusCode > 299 {
		return statusError(t, hangupResp.StatusCode, nil)
	}
	return nil
}
