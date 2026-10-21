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
