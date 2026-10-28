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
	reaped bool
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
	cmd, copied, err := u.sealedCommand(name, digest)
	if err != nil {
		return nil, err
	}
	defer copied.Close()
	cmd.Stderr, cmd.WaitDelay = io.Discard, time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	process := &NativeProcess{Stdin: stdin, Stdout: stdout, cmd: cmd, done: make(chan struct{}), cancel: cancel}
	go process.reap()
	go func() {
		select {
		case <-operation.Done():
			process.Close()
		case <-process.done:
		}
	}()
	return process, nil
}

func (p *NativeProcess) reap() {
	_ = exited(p.cmd.Process.Pid)
	p.mu.Lock()
	kill(p.cmd.Process.Pid)
	_ = p.cmd.Wait()
	p.reaped = true
	p.mu.Unlock()
	close(p.done)
}

func (p *NativeProcess) Close() error {
	p.cancel()
	p.stop.Do(func() {
		p.Stdin.Close()
		p.Stdout.Close()
		p.mu.Lock()
		if !p.reaped {
			kill(p.cmd.Process.Pid)
		}
		p.mu.Unlock()
	})
	<-p.done
	return nil
}
