package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/plugin/abi"
)

// read reads what the plugin writes to standard output: first its ABI
// version, which it reports to hello, then responses, parts of them and
// capability requests, until the plugin and whatever it started close
// standard output, or until reap stops waiting for them.
func (p *process) read(frames *bufio.Reader, hello chan<- error) {
	defer p.reading.Done()
	defer panics.Recover(p.log, "plugin reader", func(error) { p.stop("OLP could not process what it wrote") })
	frame, err := readFrame(frames)
	switch {
	case err != nil:
		hello <- refuse(CodeExecutableInvalid, "The executable does not speak the plugin ABI over standard input and output: it announced no ABI version.")
	case frame.Version != abi.Version:
		hello <- refuse(CodeABIUnsupported, fmt.Sprintf("The executable was built for plugin ABI %d; this OLP runs ABI %d.", frame.Version, abi.Version))
	default:
		hello <- nil
		if violation := p.receive(frames); violation != "" {
			p.stop("it broke the stdio protocol: " + violation)
		}
	}
	_, _ = io.Copy(io.Discard, frames)
}

// drainTime bounds how long OLP reads what an exited plugin wrote, while
// something it started outside its process group holds its output open.
const drainTime = time.Second

// reap waits for the process to exit and reaps it. It kills the process group
// first, so nothing the plugin started outlives it, and gives the readers of
// its standard output and standard error drainTime to read what it wrote, in
// case something that left the group still holds them. Then it stops the
// process, which fails the calls it left unanswered.
func (p *process) reap(stdout, stderr *os.File) {
	if err := exited(p.cmd.Process.Pid); err != nil {
		p.log.Warn("unconfined plugin could not be awaited", "error", err)
	}
	p.mu.Lock()
	kill(p.cmd.Process.Pid)
	p.exited = true
	p.mu.Unlock()
	drained := time.Now().Add(drainTime)
	_ = stdout.SetReadDeadline(drained)
	_ = stderr.SetReadDeadline(drained)
	p.reading.Wait()
	stdout.Close()
	stderr.Close()
	reason := "it exited"
	if err := p.cmd.Wait(); err != nil {
		reason += " (" + err.Error() + ")"
	}
	p.stop(reason)
}

func (p *process) acquireCapability() (chan struct{}, bool) {
	p.mu.Lock()
	if p.caps == nil {
		p.caps = make(chan struct{}, defaultLimits.Instances)
	}
	caps := p.caps
	p.mu.Unlock()
	select {
	case caps <- struct{}{}:
		return caps, true
	default:
		return nil, false
	}
}

// receive serves the plugin's frames until it closes standard output, or
// until one breaks the protocol, which it describes.
func (p *process) receive(frames *bufio.Reader) string {
	for {
		frame, err := readFrame(frames)
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded):
			// The plugin closed standard output, or reap stopped waiting
			// for what it started to.
			return ""
		case err != nil:
			return err.Error()
		case frame.Response != nil:
			p.respond(frame.ID, *frame.Response)
		case frame.Part != nil:
			if violation := p.deliver(frame.ID, frame.Part); violation != "" {
				return violation
			}
		case frame.Request != nil:
			sem, ok := p.acquireCapability()
			if !ok {
				_ = p.send(abi.Frame{ID: frame.ID, Response: &abi.Response{Error: &abi.Error{Code: abi.CodeInternal, Message: "OLP could not serve the request."}}})
				continue
			}
			ctx := p.grants(frame.Call)
			go func() {
				defer func() { <-sem }()
				defer panics.Recover(p.log, "plugin capability", func(error) {
					_ = p.send(abi.Frame{ID: frame.ID, Response: &abi.Response{Error: &abi.Error{Code: abi.CodeInternal, Message: "OLP could not serve the request."}}})
				})
				response := capability(ctx, *frame.Request)
				_ = p.send(abi.Frame{ID: frame.ID, Response: &response})
			}()
		default:
			return "a frame is neither a response, a part of one nor a capability request"
		}
	}
}

// respond hands a response the plugin sent to the call it answers.
func (p *process) respond(id uint64, response abi.Response) {
	call := p.forget(id)
	switch {
	case call == nil:
		// A call OLP no longer awaits.
	case call.result != nil:
		if call.watchdog != nil {
			call.watchdog.Stop()
		}
		call.result.end(response)
	default:
		call.watchdog.Stop()
		call.reply <- response
	}
}

// deliver hands a part the plugin sent to the call it belongs to, or
// describes how it breaks the protocol. A reader that has fallen too far
// behind the parts of its call loses the call.
func (p *process) deliver(id uint64, part json.RawMessage) string {
	p.mu.Lock()
	call := p.calls[id]
	p.mu.Unlock()
	switch {
	case call == nil:
		// A call OLP no longer awaits.
	case call.result == nil:
		return "a part answers a call whose result does not stream"
	case !call.result.deliver(part):
		go p.cancel(id, errBehind)
	}
	return ""
}

// readFrame reads the next frame the plugin wrote, skipping blank lines.
func readFrame(r *bufio.Reader) (abi.Frame, error) {
	var frame abi.Frame
	for {
		var line []byte
		for {
			chunk, err := r.ReadSlice('\n')
			if len(line)+len(chunk) > maxMessage {
				return frame, errors.New("a frame exceeds 1 MiB")
			}
			line = append(line, chunk...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				return frame, err
			}
			break
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if json.Unmarshal(line, &frame) != nil {
			return frame, errors.New("a line is not a JSON frame")
		}
		return frame, nil
	}
}

// grants returns the context that grants the capabilities of the call with
// id. A capability request serving no call, or a call no longer awaited, may
// only log, as the process does.
func (p *process) grants(id uint64) context.Context {
	p.mu.Lock()
	call := p.calls[id]
	p.mu.Unlock()
	if call != nil {
		return call.ctx
	}
	return context.WithValue(context.Background(), outputKey{}, p.output())
}

// output is the process's own output, which redacts the secret values of
// every call awaiting it, and of calls that ended within the time limit.
func (p *process) output() *output {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var secrets []string
	for _, call := range p.calls {
		secrets = append(secrets, call.secrets...)
	}
	for secret, until := range p.ended {
		if !now.After(until) {
			secrets = append(secrets, secret)
		}
	}
	if p.out == nil {
		p.out = newOutput(p.log, secrets)
	} else {
		p.out.redactSecrets(secrets)
	}
	return p.out
}

// logStderr logs what the plugin writes to standard error, a line at a time.
func (p *process) logStderr(stderr io.Reader) {
	defer p.reading.Done()
	lines := bufio.NewReaderSize(stderr, maxCallLog)
	// dropping marks an oversized line's tail: the redactor only knows
	// complete credentials, and a fragment can hold the part of one a split
	// left inside it, so nothing of an oversized line is ever recorded.
	dropping := false
	for {
		line, err := lines.ReadSlice('\n')
		complete := err == nil || errors.Is(err, io.EOF)
		if text := bytes.TrimRight(line, "\r\n"); complete && !dropping && len(text) > 0 {
			p.output().record(abi.LogRecord{Level: "info", Message: string(text), Attrs: map[string]string{"stream": "stderr"}})
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			dropping = true
		case err != nil:
			return
		default:
			dropping = false
		}
	}
}

// maxHeld bounds the bytes of a streamed result's parts that OLP holds for a
// reader that has not taken them yet, as the network would for a response
// read too slowly. The process delivers parts without waiting for their
// reader, so one slow reader never holds up the plugin's other calls.
const maxHeld = 8 << 20

// errBehind ends a streamed result whose reader fell too far behind it.
var errBehind = fmt.Errorf("the plugin streamed its result more than %d MiB ahead of its reader", maxHeld>>20)

// streamed is what the plugin sends for a call whose result streams: its
// parts, in order, then its response, or why the call ended without one.
type streamed struct {
	// out redacts what the plugin reports.
	out      *output
	mu       sync.Mutex
	parts    []json.RawMessage
	held     int
	response *abi.Response
	err      error
	// arrived holds a signal once something arrives.
	arrived chan struct{}
}

// deliver adds a part, unless the result ended. It reports false when the
// part would exceed maxHeld, which ends the result.
func (s *streamed) deliver(part json.RawMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.response != nil || s.err != nil:
		return true
	case s.held+len(part) > maxHeld:
		s.err = errBehind
		return false
	}
	s.parts, s.held = append(s.parts, part), s.held+len(part)
	s.signal()
	return true
}

// end ends the result with the call's response.
func (s *streamed) end(response abi.Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.response == nil && s.err == nil {
		s.response = &response
	}
	s.signal()
}

// fail ends the result with err, unless it ended.
func (s *streamed) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.response == nil && s.err == nil {
		s.err = err
	}
	s.signal()
}

func (s *streamed) signal() {
	select {
	case s.arrived <- struct{}{}:
	default:
	}
}

// next waits for the result's next part. Once the parts run out, it returns
// io.EOF when the plugin answered the call, the failure the plugin reported,
// redacted, or why the call ended without an answer.
func (s *streamed) next() (json.RawMessage, error) {
	for {
		s.mu.Lock()
		if len(s.parts) > 0 {
			part := s.parts[0]
			s.parts[0] = nil
			s.parts, s.held = s.parts[1:], s.held-len(part)
			s.mu.Unlock()
			return part, nil
		}
		response, err := s.response, s.err
		s.mu.Unlock()
		switch {
		case err != nil:
			return nil, err
		case response != nil:
			if err = reportedFailure(s.out, *response); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		<-s.arrived
	}
}
