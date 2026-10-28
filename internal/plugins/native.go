package plugins

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type NativeProcess struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	cmd    *exec.Cmd
	done   chan struct{}
	stop   sync.Once
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (u *Unconfined) sealedCommand(name, digest string) (*exec.Cmd, *os.File, error) {
	path, _, err := u.lookup(name)
	if isCode(err, CodeExecutableUnknown) {
		return nil, nil, refuse(CodeExecutableChanged, "The installed executable is unavailable.")
	}
	if err != nil {
		return nil, nil, err
	}
	copied, copiedDigest, err := sealed(path)
	if err != nil {
		return nil, nil, err
	}
	if copiedDigest != digest {
		copied.Close()
		return nil, nil, refuse(CodeExecutableChanged, "The installed executable no longer has its approved digest.")
	}
	cmd := command(copied, name)
	cmd.Dir, cmd.Env = u.dir, []string{}
	return cmd, copied, nil
}

func (u *Unconfined) StartNative(ctx context.Context, name, digest string, timeout time.Duration) (*NativeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd, copied, err := u.sealedCommand(name, digest)
	if err != nil {
		return nil, err
	}
	defer copied.Close()
	cmd.Stderr, cmd.WaitDelay = io.Discard, time.Second
	stdinReader, stdin, stdinErr := os.Pipe()
	if stdinErr != nil {
		return nil, stdinErr
	}
	stdout, stdoutWriter, stdoutErr := os.Pipe()
	if stdoutErr != nil {
		stdinReader.Close()
		stdin.Close()
		return nil, stdoutErr
	}
	cmd.Stdin, cmd.Stdout = stdinReader, stdoutWriter
	startErr := cmd.Start()
	stdinReader.Close()
	stdoutWriter.Close()
	if startErr != nil {
		stdin.Close()
		stdout.Close()
		return nil, startErr
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	process := &NativeProcess{Stdin: stdin, Stdout: stdout, cmd: cmd, done: make(chan struct{}), cancel: cancel}
	go process.reap(stdout)
	go func() { <-operation.Done(); process.Close() }()

	return process, nil
}

func (p *NativeProcess) reap(stdout *os.File) {
	_ = exited(p.cmd.Process.Pid)
	p.mu.Lock()
	kill(p.cmd.Process.Pid)
	_ = stdout.SetReadDeadline(time.Now().Add(drainTime))
	_ = p.cmd.Wait()
	close(p.done)
	p.mu.Unlock()
}

func (p *NativeProcess) Close() error {
	p.cancel()
	p.stop.Do(func() {
		p.Stdin.Close()
		p.Stdout.Close()
		p.mu.Lock()
		select {
		case <-p.done:
		default:
			kill(p.cmd.Process.Pid)
		}
		p.mu.Unlock()
	})
	<-p.done
	return nil
}
