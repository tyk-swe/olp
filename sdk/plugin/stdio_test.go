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
	if answered := o.read(); answered.ID != 2 || answered.Response == nil || string(answered.Response.Result) != `{"headers":{"X-Signature":"sk-acme"}}` {
		t.Fatalf("call 2 answered %+v", answered)
	}
	o.write(abi.Frame{ID: 1, Cancel: true})
	if answered := o.read(); answered.ID != 1 || answered.Response == nil || answered.Response.Error == nil || answered.Response.Error.Code != "cancelled" {
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
	if answered := o.read(); answered.ID != 7 || answered.Response == nil || answered.Response.Error != nil {
		t.Fatalf("call 7 answered %+v", answered)
	}
}

// Outside OLP, such as in a plugin's own tests, capabilities are unavailable.
func TestCapabilitiesAreUnavailableOutsideOLP(t *testing.T) {
	if _, err := callHost(context.Background(), abi.CapabilityLog, abi.LogRecord{Message: "hi"}); err == nil {
		t.Fatal("a capability was available outside OLP")
	}
}
