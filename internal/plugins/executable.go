package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Executable is an unconfined plugin, which OLP runs as a subprocess of its
// executable speaking the plugin ABI over standard input and output. One
// process serves all its calls, concurrently. OLP starts it for the first
// call, from a sealed copy of the executable whose digest it checked, and
// starts it again for the next call after it exits or is stopped. A caller
// that stops waiting for a call cancels it. A plugin that leaves a call
// unanswered past the time limit, cancelled or not, may be stuck, so OLP stops
// it with whatever it started, which fails the calls in flight on it. A call
// whose result streams instead lasts as long as its caller waits, and is given
// the time limit to answer once its caller stops waiting.
type Executable struct {
	// Digest is the lowercase hexadecimal SHA-256 digest of the executable.
	Digest  string
	name    string
	tier    *Unconfined
	mu      sync.Mutex
	running *process
	// starting is the start of a process that calls are waiting for, if
	// one is under way.
	starting *launch
	closed   bool
}

// A launch is one start of a plugin's process, which every call waiting for a
// process shares.
type launch struct {
	done    chan struct{}
	process *process
	err     error
}

// Call serves call on the plugin's process and decodes its result into
// result, as Module.Call does. A call the process leaves unanswered fails
// with an *Error: plugin_timed_out once the time limit passed, and
// plugin_failed when the process stopped first.
func (e *Executable) Call(ctx context.Context, call Call, result any) error {
	request, err := call.request()
	if err != nil {
		return err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, e.tier.limits.Time)
	defer cancel()
	out := call.output(e.tier.log, e.Digest)
	defer out.close()
	p, err := e.process(ctx)
	var response abi.Response
	if err == nil {
		response, err = p.call(call.context(ctx, out), request, call.Secrets)
	}
	switch {
	case err == nil:
		return decodeResult(out, call.Method, response, result)
	case isReported(err):
		return err
	case parent.Err() != nil:
		return parent.Err()
	case ctx.Err() != nil:
		return refuse(CodeTimedOut, fmt.Sprintf("The plugin exceeded its %s time limit.", e.tier.limits.Time))
	}
	return refuse(CodeFailed, "The plugin stopped: "+err.Error()+".")
}

// stream serves call, whose result the plugin streams in parts before its
// response, on the plugin's process, and returns what the plugin sends for it.
// Starting the process takes at most the time limit. The call lasts until ctx
// ends, which cancels it.
func (e *Executable) stream(ctx context.Context, call Call) (*streamed, error) {
	request, err := call.request()
	if err != nil {
		return nil, err
	}
	start, cancel := context.WithTimeout(ctx, e.tier.limits.Time)
	defer cancel()
	p, err := e.process(start)
	switch {
	case err == nil:
	case isReported(err):
		return nil, err
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case start.Err() != nil:
		return nil, refuse(CodeTimedOut, fmt.Sprintf("The plugin did not start within its %s time limit.", e.tier.limits.Time))
	default:
		return nil, refuse(CodeFailed, "The plugin stopped: "+err.Error()+".")
	}
	out := call.output(e.tier.log, e.Digest)
	context.AfterFunc(ctx, out.close)
	result, err := p.stream(call.context(ctx, out), request, call.Secrets)
	if err != nil {
		return nil, refuse(CodeFailed, "The plugin stopped: "+err.Error()+".")
	}
	return result, nil
}

// Close stops the plugin's process. A call still running fails.
func (e *Executable) Close(context.Context) error {
	e.mu.Lock()
	e.closed = true
	running := e.running
	e.mu.Unlock()
	if running != nil {
		running.stop(errClosed.Error())
	}
	return nil
}

// process returns the plugin's running process, starting one if none runs,
// or waits for the start under way. A caller gives up waiting when ctx ends,
// which leaves the start to the others.
func (e *Executable) process(ctx context.Context) (*process, error) {
	e.mu.Lock()
	switch {
	case e.closed:
		e.mu.Unlock()
		return nil, errClosed
	case e.running != nil && !e.running.stopped():
		defer e.mu.Unlock()
		return e.running, nil
	case e.starting == nil:
		e.starting = &launch{done: make(chan struct{})}
		go e.launch(e.starting)
	}
	started := e.starting
	e.mu.Unlock()
	select {
	case <-started.done:
		return started.process, started.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// errClosed fails the calls of an Executable that OLP closed.
var errClosed = errors.New("OLP closed it")

// launch starts a process for the calls waiting for l, within the time limit
// whether or not any still waits.
func (e *Executable) launch(l *launch) {
	ctx, cancel := context.WithTimeout(context.Background(), e.tier.limits.Time)
	defer cancel()
	p, err := e.tier.start(ctx, e.name, e.Digest)
	if err != nil && ctx.Err() != nil {
		err = refuse(CodeTimedOut, fmt.Sprintf("The plugin did not start within its %s time limit.", e.tier.limits.Time))
	}
	e.mu.Lock()
	e.starting = nil
	switch {
	case err != nil:
	case e.closed:
		p.stop(errClosed.Error())
		p, err = nil, errClosed
	default:
		e.running = p
	}
	e.mu.Unlock()
	l.process, l.err = p, err
	close(l.done)
}

// process is one run of an unconfined plugin's executable.
type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	log     *slog.Logger
	limit   time.Duration
	writing sync.Mutex
	// reading counts the readers of the plugin's standard output and
	// standard error.
	reading sync.WaitGroup
	mu      sync.Mutex
	// calls are the calls awaiting the plugin's response, by ID.
	calls map[uint64]*pending
	next  uint64
	// ended holds the secret values of calls that ended, until when OLP
	// still redacts them from the plugin's standard error.
	ended map[string]time.Time
	// out retains the process's allowance for stderr and context-free logs.
	out *output
	// reason says why the process stopped; done is closed once it is set.
	reason string
	done   chan struct{}
	// exited is set once the process exited, after which its ID no longer
	// names its process group.
	exited bool
}

// pending is a call awaiting the plugin's response.
type pending struct {
	// ctx grants the call's capabilities.
	ctx     context.Context
	secrets []string
	reply   chan abi.Response
	// result receives what the plugin sends for a call whose result streams,
	// instead of reply.
	result *streamed
	// watchdog stops the process if the plugin leaves the call unanswered
	// too long; a streamed call has one once it is cancelled.
	watchdog *time.Timer
}

// start starts the executable with name, which must have digest, with no
// arguments and an empty environment in the unconfined plugin directory, and
// waits for it to announce its ABI version. It runs a sealed copy of the
// executable, whose digest it checks, in a process group of its own.
func (u *Unconfined) start(ctx context.Context, name, digest string) (*process, error) {
	path, _, err := u.lookup(name)
	if refusal, ok := errors.AsType[*Error](err); ok && refusal.Code == CodeExecutableUnknown {
		return nil, refuse(CodeExecutableChanged, "The unconfined plugin directory no longer holds the plugin's executable.")
	}
	if err != nil {
		return nil, err
	}
	copied, copiedDigest, err := sealed(path)
	if err != nil {
		return nil, err
	}
	defer copied.Close()
	if copiedDigest != digest {
		return nil, refuse(CodeExecutableChanged, "The executable no longer has the plugin's digest. An owner permits each build of an unconfined plugin.")
	}
	cmd := command(copied, name)
	cmd.Dir, cmd.Env = u.dir, []string{}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// OLP owns the read ends of these pipes, so it reaps the process as soon
	// as it exits, whoever else holds the write ends.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		stdout.Close()
		stdoutWriter.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	err = cmd.Start()
	stdoutWriter.Close()
	stderrWriter.Close()
	if err != nil {
		stdout.Close()
		stderr.Close()
		return nil, refuse(CodeExecutableInvalid, "OLP could not start the executable: "+err.Error())
	}
	p := &process{cmd: cmd, stdin: stdin, log: u.log.With("plugin_digest", digest), limit: u.limits.Time,
		calls: map[uint64]*pending{}, ended: map[string]time.Time{}, done: make(chan struct{})}
	p.reading.Add(2)
	go p.logStderr(stderr)
	hello := make(chan error, 1)
	go p.read(bufio.NewReaderSize(stdout, 64<<10), hello)
	go p.reap(stdout, stderr)
	select {
	case err = <-hello:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		p.stop("it did not start")
		return nil, err
	}
	return p, nil
}

// call sends the plugin a call and waits for its response. The call's
// context grants its capabilities; when it ends first, whether its caller
// cancelled it or its caller's deadline passed, OLP cancels the call.
func (p *process) call(ctx context.Context, request abi.Request, secrets []string) (abi.Response, error) {
	call := &pending{ctx: ctx, secrets: secrets, reply: make(chan abi.Response, 1)}
	p.mu.Lock()
	if p.reason != "" {
		p.mu.Unlock()
		return abi.Response{}, p.failure()
	}
	p.next++
	id := p.next
	// The plugin answers every call within its time limit, even a cancelled
	// one; a plugin that doesn't may be stuck. A caller's own deadline only
	// cancels its call. The call's context, which lasts at most the time
	// limit, has ended by the time the plugin is stopped, so its caller
	// reports the time limit.
	call.watchdog = time.AfterFunc(p.limit, func() {
		<-ctx.Done()
		if p.forget(id) != nil {
			p.stop(fmt.Sprintf("it left a call unanswered past its %s time limit", p.limit))
		}
	})
	p.calls[id] = call
	p.mu.Unlock()
	if err := p.send(abi.Frame{ID: id, Request: &request}); err != nil {
		p.stop("OLP could not write to it")
	}
	select {
	case response := <-call.reply:
		return response, nil
	case <-p.done:
		return abi.Response{}, p.failure()
	case <-ctx.Done():
		_ = p.send(abi.Frame{ID: id, Cancel: true})
		return abi.Response{}, ctx.Err()
	}
}

// stream sends the plugin a call whose result streams and returns what the
// plugin sends for it. The call's context grants its capabilities; once it
// ends, OLP cancels the call.
func (p *process) stream(ctx context.Context, request abi.Request, secrets []string) (*streamed, error) {
	call := &pending{ctx: ctx, secrets: secrets, result: &streamed{out: callOutput(ctx), arrived: make(chan struct{}, 1)}}
	p.mu.Lock()
	if p.reason != "" {
		p.mu.Unlock()
		return nil, p.failure()
	}
	p.next++
	id := p.next
	p.calls[id] = call
	p.mu.Unlock()
	// Arm cancellation before writing: a carrier may stop reading stdin,
	// holding send and every other writer until the watchdog stops it.
	context.AfterFunc(ctx, func() { p.cancel(id, ctx.Err()) })
	if err := p.send(abi.Frame{ID: id, Request: &request}); err != nil {
		p.stop("OLP could not write to it")
		return nil, p.failure()
	}
	return call.result, nil
}

// cancel cancels the streamed call id, which OLP no longer awaits, ending
// its result with err. The plugin still answers the call; one that leaves it
// unanswered past the time limit may be stuck, so OLP stops it.
func (p *process) cancel(id uint64, err error) {
	p.mu.Lock()
	call := p.calls[id]
	if call == nil || call.watchdog != nil {
		p.mu.Unlock()
		return
	}
	call.watchdog = time.AfterFunc(p.limit, func() {
		if p.forget(id) != nil {
			p.stop(fmt.Sprintf("it left a cancelled call unanswered past its %s time limit", p.limit))
		}
	})
	p.mu.Unlock()
	call.result.fail(err)
	_ = p.send(abi.Frame{ID: id, Cancel: true})
}

// forget returns the pending call with id, which no longer awaits a response,
// or nil when there is none.
func (p *process) forget(id uint64) *pending {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := p.calls[id]
	if call != nil {
		delete(p.calls, id)
		p.end(call)
	}
	return call
}

// end keeps the secret values of a call that ended for redaction, for the
// time limit: the plugin writes its standard error on another pipe than its
// responses, so what it wrote there while serving the call may reach OLP
// after the response. It lets go of those kept longer. p.mu is held.
func (p *process) end(call *pending) {
	now := time.Now()
	for secret, until := range p.ended {
		if now.After(until) {
			delete(p.ended, secret)
		}
	}
	for _, secret := range call.secrets {
		p.ended[secret] = now.Add(p.limit)
	}
}

func (p *process) send(frame abi.Frame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	p.writing.Lock()
	defer p.writing.Unlock()
	_, err = p.stdin.Write(append(data, '\n'))
	return err
}

// stop kills the process for reason, with whatever it started, and fails the
// calls awaiting it.
func (p *process) stop(reason string) {
	p.mu.Lock()
	if p.reason != "" {
		p.mu.Unlock()
		return
	}
	p.reason = reason
	calls := p.calls
	p.calls = nil
	for _, call := range calls {
		p.end(call)
	}
	if !p.exited {
		kill(p.cmd.Process.Pid)
	}
	p.mu.Unlock()
	// Closing the host's pipe interrupts a blocked write even if another
	// process still holds the plugin's read end.
	_ = p.stdin.Close()
	for _, call := range calls {
		if call.watchdog != nil {
			call.watchdog.Stop()
		}
		if call.result != nil {
			call.result.fail(refuse(CodeFailed, "The plugin stopped: "+reason+"."))
		}
	}
	close(p.done)
}

func (p *process) stopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason != ""
}

// failure is why the process stopped.
func (p *process) failure() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.New(p.reason)
}

// read reads what the plugin writes to standard output: first its ABI
// version, which it reports to hello, then responses and capability
// requests, until the plugin and whatever it started close standard output,
// or until reap stops waiting for them.
func (p *process) read(frames *bufio.Reader, hello chan<- error) {
	defer p.reading.Done()
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
			call := p.forget(frame.ID)
			switch {
			case call == nil:
			case call.result != nil:
				if call.watchdog != nil {
					call.watchdog.Stop()
				}
				call.result.end(*frame.Response)
			default:
				call.watchdog.Stop()
				call.reply <- *frame.Response
			}
		case frame.Part != nil:
			if violation := p.deliver(frame.ID, frame.Part); violation != "" {
				return violation
			}
		case frame.Request != nil:
			ctx := p.grants(frame.Call)
			go func() {
				response := capability(ctx, *frame.Request)
				_ = p.send(abi.Frame{ID: frame.ID, Response: &response})
			}()
		default:
			return "a frame is neither a response, a part of one nor a capability request"
		}
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
