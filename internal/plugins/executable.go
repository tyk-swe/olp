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
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Executable is an unconfined plugin, which OLP runs as a subprocess of its
// executable speaking the plugin ABI over standard input and output. One
// process serves all its calls, concurrently. OLP starts it for the first
// call, after checking that the executable still has its digest, and starts
// it again for the next call after it exits or is stopped. A plugin that
// leaves a call unanswered past the time limit, cancelled or not, may be
// stuck, so OLP stops it, which fails the calls in flight on it.
type Executable struct {
	// Digest is the lowercase hexadecimal SHA-256 digest of the executable.
	Digest  string
	name    string
	tier    *Unconfined
	mu      sync.Mutex
	running *process
	closed  bool
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
	out := newOutput(e.tier.log.With("plugin_digest", e.Digest, "plugin_method", call.Method), call.Secrets)
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

// Close stops the plugin's process. A call still running fails.
func (e *Executable) Close(context.Context) error {
	e.mu.Lock()
	e.closed = true
	running := e.running
	e.mu.Unlock()
	if running != nil {
		running.stop("OLP closed it")
	}
	return nil
}

// process returns the plugin's running process, starting one if none runs.
func (e *Executable) process(ctx context.Context) (*process, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("OLP closed it")
	}
	if e.running != nil && !e.running.stopped() {
		return e.running, nil
	}
	p, err := e.tier.start(ctx, e.name, e.Digest)
	if err != nil {
		return nil, err
	}
	e.running = p
	return p, nil
}

// process is one run of an unconfined plugin's executable.
type process struct {
	cmd     *exec.Cmd
	stdin   io.Writer
	log     *slog.Logger
	limit   time.Duration
	writing sync.Mutex
	mu      sync.Mutex
	// calls are the calls awaiting the plugin's response, by ID.
	calls map[uint64]*pending
	next  uint64
	// reason says why the process stopped; done is closed once it is set.
	reason     string
	done       chan struct{}
	stderrDone chan struct{}
}

// pending is a call awaiting the plugin's response.
type pending struct {
	// ctx grants the call's capabilities.
	ctx      context.Context
	secrets  []string
	reply    chan abi.Response
	watchdog *time.Timer
}

// start starts the executable with name, which must have digest, with no
// arguments and an empty environment in the unconfined plugin directory, and
// waits for it to announce its ABI version.
func (u *Unconfined) start(ctx context.Context, name, digest string) (*process, error) {
	file, err := u.executable(name)
	if refusal, ok := errors.AsType[*Error](err); ok && refusal.Code == CodeExecutableUnknown {
		return nil, refuse(CodeExecutableChanged, "The unconfined plugin directory no longer holds the plugin's executable.")
	}
	if err != nil {
		return nil, err
	}
	if file.Digest != digest {
		return nil, refuse(CodeExecutableChanged, "The executable no longer has the plugin's digest. An owner permits each build of an unconfined plugin.")
	}
	cmd := exec.Command(filepath.Join(u.dir, name))
	cmd.Dir, cmd.Env = u.dir, []string{}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, refuse(CodeExecutableInvalid, "OLP could not start the executable: "+err.Error())
	}
	p := &process{cmd: cmd, stdin: stdin, log: u.log.With("plugin_digest", digest), limit: u.limits.Time,
		calls: map[uint64]*pending{}, done: make(chan struct{}), stderrDone: make(chan struct{})}
	go p.logStderr(stderr)
	hello := make(chan error, 1)
	go p.read(bufio.NewReaderSize(stdout, 64<<10), hello)
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
// context grants its capabilities; when it ends first, OLP cancels the call.
func (p *process) call(ctx context.Context, request abi.Request, secrets []string) (abi.Response, error) {
	call := &pending{ctx: ctx, secrets: secrets, reply: make(chan abi.Response, 1)}
	deadline, _ := ctx.Deadline()
	p.mu.Lock()
	if p.reason != "" {
		p.mu.Unlock()
		return abi.Response{}, p.failure()
	}
	p.next++
	id := p.next
	// The plugin answers every call within the time limit, even a cancelled
	// one; a plugin that doesn't may be stuck. The call's context has ended
	// by the time it is stopped, so its caller reports the time limit.
	call.watchdog = time.AfterFunc(time.Until(deadline), func() {
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
		if errors.Is(ctx.Err(), context.Canceled) {
			_ = p.send(abi.Frame{ID: id, Cancel: true})
		}
		return abi.Response{}, ctx.Err()
	}
}

// forget returns the pending call with id, which no longer awaits a response,
// or nil when there is none.
func (p *process) forget(id uint64) *pending {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := p.calls[id]
	delete(p.calls, id)
	return call
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

// stop kills the process for reason and fails the calls awaiting it.
func (p *process) stop(reason string) {
	p.mu.Lock()
	if p.reason != "" {
		p.mu.Unlock()
		return
	}
	p.reason = reason
	calls := p.calls
	p.calls = nil
	p.mu.Unlock()
	for _, call := range calls {
		call.watchdog.Stop()
	}
	close(p.done)
	_ = p.cmd.Process.Kill()
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
// requests. Once the plugin closes standard output, it reaps the process.
func (p *process) read(frames *bufio.Reader, hello chan<- error) {
	reason := "it exited"
	frame, err := readFrame(frames)
	switch {
	case err != nil:
		hello <- refuse(CodeExecutableInvalid, "The executable does not speak the plugin ABI over standard input and output: it announced no ABI version.")
	case frame.Version != abi.Version:
		hello <- refuse(CodeABIUnsupported, fmt.Sprintf("The executable was built for plugin ABI %d; this OLP runs ABI %d.", frame.Version, abi.Version))
	default:
		hello <- nil
		if violation := p.receive(frames); violation != "" {
			reason = "it broke the stdio protocol: " + violation
			p.stop(reason)
		}
	}
	_, _ = io.Copy(io.Discard, frames)
	<-p.stderrDone
	if err = p.cmd.Wait(); err != nil {
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
		case errors.Is(err, io.EOF):
			return ""
		case err != nil:
			return err.Error()
		case frame.Response != nil:
			if call := p.forget(frame.ID); call != nil {
				call.watchdog.Stop()
				call.reply <- *frame.Response
			}
		case frame.Request != nil:
			ctx := p.grants(frame.Call)
			go func() {
				response := capability(ctx, *frame.Request)
				_ = p.send(abi.Frame{ID: frame.ID, Response: &response})
			}()
		default:
			return "a frame is neither a response nor a capability request"
		}
	}
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
// every call awaiting it.
func (p *process) output() *output {
	p.mu.Lock()
	var secrets []string
	for _, call := range p.calls {
		secrets = append(secrets, call.secrets...)
	}
	p.mu.Unlock()
	return newOutput(p.log, secrets)
}

// logStderr logs what the plugin writes to standard error, a line at a time.
func (p *process) logStderr(stderr io.Reader) {
	defer close(p.stderrDone)
	lines := bufio.NewReaderSize(stderr, maxCallLog)
	dropping := false
	for {
		line, err := lines.ReadSlice('\n')
		if text := bytes.TrimRight(line, "\r\n"); !dropping && len(text) > 0 {
			p.output().record(abi.LogRecord{Level: "info", Message: string(text), Attrs: map[string]string{"stream": "stderr"}})
		}
		// A line too long to redact whole is logged by its start and the
		// rest of it dropped, so no secret is split across records.
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
