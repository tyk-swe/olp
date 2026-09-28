//go:build !wasip1

package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// olp is the host end of a plugin's stdio transport.
type olp struct {
	t      *testing.T
	in     *io.PipeWriter
	frames *bufio.Scanner
}

func serveOverStdio(t *testing.T, p Plugin) *olp {
	t.Helper()
	registered = p
	inReader, in := io.Pipe()
	out, outWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveFrames(inReader, outWriter)
		outWriter.Close()
	}()
	t.Cleanup(func() {
		in.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
		registered = nil
	})
	return &olp{t: t, in: in, frames: bufio.NewScanner(out)}
}

func (o *olp) write(frame abi.Frame) {
	o.t.Helper()
	data, _ := json.Marshal(frame)
	if _, err := o.in.Write(append(data, '\n')); err != nil {
		o.t.Fatal(err)
	}
}

func (o *olp) read() abi.Frame {
	o.t.Helper()
	if !o.frames.Scan() {
		o.t.Fatalf("the plugin wrote no frame: %v", o.frames.Err())
	}
	var frame abi.Frame
	if err := json.Unmarshal(o.frames.Bytes(), &frame); err != nil {
		o.t.Fatalf("frame %s: %v", o.frames.Bytes(), err)
	}
	return frame
}

// response reads the next frame, which must be the response to call id.
func (o *olp) response(id uint64) abi.Response {
	o.t.Helper()
	frame := o.read()
	if frame.ID != id || frame.Response == nil {
		o.t.Fatalf("frame %+v, want the response to call %d", frame, id)
	}
	return *frame.Response
}

func signCall(id uint64, credential string) abi.Frame {
	params, _ := json.Marshal(SignRequest{Profile: "acme-chat", Method: "POST", URL: "https://api.acme.example/v1", Credential: credential})
	return abi.Frame{ID: id, Request: &abi.Request{Method: abi.MethodSign, Params: params, Provider: &Provider{Profile: "acme-chat"}}}
}

// Over stdio a plugin announces its ABI version, then serves calls
// concurrently: a call waiting for its cancellation doesn't hold up the
// next, and the plugin still answers it once OLP cancels it.
func TestServeAnswersCallsConcurrentlyOverStdio(t *testing.T) {
	o := serveOverStdio(t, signing{sign: func(ctx context.Context, r SignRequest) (SignResult, error) {
		if r.Credential == "wait" {
			<-ctx.Done()
			return SignResult{}, &Error{Code: "cancelled", Message: "OLP cancelled the call."}
		}
		return SignResult{Headers: map[string]string{"X-Signature": r.Credential}}, nil
	}})
	if hello := o.read(); hello.Version != abi.Version || hello.ID != 0 {
		t.Fatalf("first frame %+v", hello)
	}
	o.write(signCall(1, "wait"))
	o.write(signCall(2, "sk-acme"))
	if answered := o.response(2); string(answered.Result) != `{"headers":{"X-Signature":"sk-acme"}}` {
		t.Fatalf("call 2 answered %+v", answered)
	}
	o.write(abi.Frame{ID: 1, Cancel: true})
	if answered := o.response(1); answered.Error == nil || answered.Error.Code != "cancelled" {
		t.Fatalf("call 1 answered %+v", answered)
	}
}

// A capability request names the call it serves, whose context the plugin
// passed, and the call continues once OLP answers it.
func TestCapabilityRequestsNameTheirCallOverStdio(t *testing.T) {
	o := serveOverStdio(t, signing{sign: func(ctx context.Context, r SignRequest) (SignResult, error) {
		Log.InfoContext(ctx, "signing", "url", r.URL)
		Log.Info("between calls")
		return SignResult{Headers: map[string]string{"X-Signature": "signed"}}, nil
	}})
	o.read()
	o.write(signCall(7, "sk-acme"))
	for _, want := range []struct {
		call    uint64
		message string
	}{{7, "signing"}, {0, "between calls"}} {
		request := o.read()
		var record abi.LogRecord
		if request.Request == nil || request.Request.Method != abi.CapabilityLog || request.Call != want.call || json.Unmarshal(request.Request.Params, &record) != nil || record.Message != want.message {
			t.Fatalf("capability request %+v, want a log for call %d", request, want.call)
		}
		o.write(abi.Frame{ID: request.ID, Response: &abi.Response{}})
	}
	if answered := o.response(7); answered.Error != nil {
		t.Fatalf("call 7 answered %+v", answered)
	}
}

// Outside OLP, such as in a plugin's own tests, capabilities are unavailable.
func TestCapabilitiesAreUnavailableOutsideOLP(t *testing.T) {
	if _, err := callHost(context.Background(), abi.CapabilityLog, abi.LogRecord{Message: "hi"}); err == nil {
		t.Fatal("a capability was available outside OLP")
	}
}

// carrying is a plugin whose Carrier answers with carry.
type carrying struct {
	manifestOnly
	carry func(context.Context, HTTPRequest) (CarriedResponse, error)
}

func (c carrying) Carry(ctx context.Context, r HTTPRequest) (CarriedResponse, error) {
	return c.carry(ctx, r)
}

func carryCall(id uint64, url string) abi.Frame {
	params, _ := json.Marshal(HTTPRequest{Method: "POST", URL: url, Header: map[string][]string{"Authorization": {"Bearer sk-acme"}}, Body: []byte(`{"stream":true}`)})
	return abi.Frame{ID: id, Request: &abi.Request{Method: abi.MethodCarry, Params: params, Provider: &Provider{Profile: "acme-chat"}}}
}

// part reads the next frame, which must be a part of call id.
func (o *olp) part(id uint64) HTTPResponse {
	o.t.Helper()
	frame := o.read()
	var part HTTPResponse
	if frame.ID != id || frame.Part == nil || json.Unmarshal(frame.Part, &part) != nil {
		o.t.Fatalf("frame %+v, want a part of call %d", frame, id)
	}
	return part
}

// A Carrier's response streams to OLP as parts of its call: its head, then
// its body as the plugin reads it, so each event reaches OLP before the
// upstream sends the next. The call's response then ends it.
func TestCarrierStreamsTheResponseOverStdio(t *testing.T) {
	body, upstream := io.Pipe()
	var carried HTTPRequest
	var provider Provider
	o := serveOverStdio(t, carrying{carry: func(ctx context.Context, r HTTPRequest) (CarriedResponse, error) {
		carried = r
		provider, _ = ProviderOf(ctx)
		return CarriedResponse{Status: 200, Header: map[string][]string{"Content-Type": {"text/event-stream"}}, Body: body}, nil
	}})
	o.read()
	o.write(carryCall(3, "https://api.acme.example/v1/chat/completions"))
	if head := o.part(3); head.Status != 200 || head.Header["Content-Type"][0] != "text/event-stream" || head.Body != nil {
		t.Fatalf("head %+v", head)
	}
	if carried.URL != "https://api.acme.example/v1/chat/completions" || carried.Header["Authorization"][0] != "Bearer sk-acme" || string(carried.Body) != `{"stream":true}` || provider.Profile != "acme-chat" {
		t.Fatalf("carried %+v for %+v", carried, provider)
	}
	for _, event := range []string{"data: {\"n\":1}\n\n", "data: [DONE]\n\n"} {
		go upstream.Write([]byte(event))
		if part := o.part(3); string(part.Body) != event || part.Status != 0 {
			t.Fatalf("part %+v, want %q", part, event)
		}
	}
	upstream.Close()
	if answered := o.response(3); answered.Error != nil {
		t.Fatalf("call 3 answered %+v", answered)
	}
}

// OLP's cancellation of a carry call reaches the Carrier's context and closes
// a body the plugin is reading, and what the plugin reports ends the call: a
// request it never sent as not sent, any other failure as a failure.
func TestCarryCancellationReachesTheCarrier(t *testing.T) {
	body, _ := io.Pipe()
	o := serveOverStdio(t, carrying{carry: func(ctx context.Context, r HTTPRequest) (CarriedResponse, error) {
		if r.URL == "https://api.acme.example/connecting" {
			<-ctx.Done()
			return CarriedResponse{}, &Error{Code: abi.CodeNotSent, Message: "cancelled while connecting"}
		}
		return CarriedResponse{Status: 200, Body: body}, nil
	}})
	o.read()
	o.write(carryCall(1, "https://api.acme.example/connecting"))
	o.write(abi.Frame{ID: 1, Cancel: true})
	if answered := o.response(1); answered.Error == nil || answered.Error.Code != abi.CodeNotSent {
		t.Fatalf("call 1 answered %+v", answered)
	}
	o.write(carryCall(2, "https://api.acme.example/streaming"))
	if head := o.part(2); head.Status != 200 {
		t.Fatalf("head %+v", head)
	}
	o.write(abi.Frame{ID: 2, Cancel: true})
	if answered := o.response(2); answered.Error == nil || answered.Error.Code != abi.CodeInternal {
		t.Fatalf("call 2 answered %+v", answered)
	}
}

// A plugin can't declare a profile that carries traffic without a Carrier:
// it reports no manifest, so OLP never permits it.
func TestServeRefusesACarryingProfileWithoutACarrier(t *testing.T) {
	declared := Manifest{Name: "acme", Version: "1.0.0", Profiles: []Profile{{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", CarriesTraffic: true}}}
	if response := serveWith(t, manifestOnly(declared), `{"method":"manifest"}`); response.Error == nil || response.Error.Code != abi.CodeInternal {
		t.Fatalf("manifest response %+v", response)
	}
	if response := serveWith(t, carrying{manifestOnly: manifestOnly(declared)}, `{"method":"manifest"}`); response.Error != nil {
		t.Fatalf("manifest response %+v", response)
	}
}
