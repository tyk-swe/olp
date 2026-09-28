//go:build !wasip1

package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// maxFrame bounds a frame the plugin writes, as OLP does.
const maxFrame = 1 << 20

// Serve runs the plugin as an unconfined plugin: it serves OLP's calls, each
// on a goroutine of its own, over standard input and output until OLP closes
// standard input. Call it from main.
//
// Standard output carries only the ABI's frames, so Serve points os.Stdout at
// standard error, which OLP logs: what the plugin prints never breaks the
// protocol.
func Serve() {
	frames := os.Stdout
	os.Stdout = os.Stderr
	if err := serveFrames(os.Stdin, frames); err != nil {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		os.Exit(1)
	}
}

// session is the stdio transport Serve runs.
type session struct {
	out     io.Writer
	writing sync.Mutex
	mu      sync.Mutex
	// calls cancel the calls from OLP that are running, by ID.
	calls map[uint64]context.CancelFunc
	// requests receive OLP's responses to the capability requests awaiting
	// them, by ID.
	requests map[uint64]chan abi.Response
	next     uint64
}

// serving is the session Serve runs, if any.
var serving atomic.Pointer[session]

// callKey holds the ID of the call from OLP a context belongs to.
type callKey struct{}

// serveFrames serves the frames OLP writes to in, writing the plugin's to out,
// until in ends.
func serveFrames(in io.Reader, out io.Writer) error {
	s := &session{out: out, calls: map[uint64]context.CancelFunc{}, requests: map[uint64]chan abi.Response{}}
	serving.Store(s)
	defer serving.Store(nil)
	if err := s.send(abi.Frame{Version: abi.Version}); err != nil {
		return err
	}
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.receive(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// receive handles one frame from OLP: a call to serve, a response to a
// capability request, or a cancellation.
func (s *session) receive(line []byte) {
	var frame abi.Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		fmt.Fprintln(os.Stderr, "plugin: OLP wrote a frame that is not JSON:", err)
		return
	}
	switch {
	case frame.Request != nil:
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), callKey{}, frame.ID))
		s.mu.Lock()
		s.calls[frame.ID] = cancel
		s.mu.Unlock()
		go s.serve(ctx, cancel, frame.ID, *frame.Request)
	case frame.Response != nil:
		s.mu.Lock()
		waiting := s.requests[frame.ID]
		delete(s.requests, frame.ID)
		s.mu.Unlock()
		if waiting != nil {
			waiting <- *frame.Response
		}
	case frame.Cancel:
		s.mu.Lock()
		cancel := s.calls[frame.ID]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

// serve answers call id, whose context cancel ends.
func (s *session) serve(ctx context.Context, cancel context.CancelFunc, id uint64, request abi.Request) {
	response := handle(ctx, request)
	s.mu.Lock()
	delete(s.calls, id)
	s.mu.Unlock()
	cancel()
	frame := abi.Frame{ID: id, Response: &response}
	if data, err := json.Marshal(frame); err != nil || len(data) >= maxFrame {
		frame.Response = &abi.Response{Error: &abi.Error{Code: abi.CodeInternal, Message: "The plugin's result exceeds 1 MiB."}}
	}
	if err := s.send(frame); err != nil {
		fmt.Fprintln(os.Stderr, "plugin: could not answer OLP:", err)
	}
}

func (s *session) send(frame abi.Frame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	return s.write(data)
}

// write writes one frame, encoded.
func (s *session) write(frame []byte) error {
	s.writing.Lock()
	defer s.writing.Unlock()
	_, err := s.out.Write(append(frame, '\n'))
	return err
}

// sendPart streams one part of the result of the call ctx belongs to.
func sendPart(ctx context.Context, part any) error {
	s := serving.Load()
	if s == nil {
		return &abi.Error{Code: "unavailable", Message: "Streaming a result is available only inside OLP."}
	}
	data, err := json.Marshal(part)
	if err != nil {
		return err
	}
	call, _ := ctx.Value(callKey{}).(uint64)
	if data, err = json.Marshal(abi.Frame{ID: call, Part: data}); err != nil {
		return err
	}
	if len(data) >= maxFrame {
		return &abi.Error{Code: abi.CodeInternal, Message: "A part of the plugin's result exceeds 1 MiB."}
	}
	return s.write(data)
}

// hostCall passes a capability request to OLP for the call ctx belongs to.
// Outside OLP, such as in a plugin's own tests, it answers that capabilities
// are unavailable.
func hostCall(ctx context.Context, request abi.Request) abi.Response {
	s := serving.Load()
	if s == nil {
		return answer(nil, &abi.Error{Code: "unavailable", Message: "OLP capabilities are available only inside OLP."})
	}
	call, _ := ctx.Value(callKey{}).(uint64)
	waiting := make(chan abi.Response, 1)
	s.mu.Lock()
	s.next++
	id := s.next
	s.requests[id] = waiting
	s.mu.Unlock()
	forget := func() {
		s.mu.Lock()
		delete(s.requests, id)
		s.mu.Unlock()
	}
	if err := s.send(abi.Frame{ID: id, Call: call, Request: &request}); err != nil {
		forget()
		return answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "The plugin could not reach OLP: " + err.Error()})
	}
	select {
	case response := <-waiting:
		return response
	case <-ctx.Done():
		forget()
		return answer(nil, &abi.Error{Code: abi.CodeInternal, Message: "The call ended before OLP answered."})
	}
}
