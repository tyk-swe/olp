package testutil

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type Process struct {
	PublicOrigin    string
	PrivateOrigin   string
	GatewayInstance string
	cmd             *exec.Cmd
	done            chan struct{}
	result          error
	logPath         string
	killed          atomic.Bool
}

func Environment(values map[string]string) []string {
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "OLP_") {
			env = append(env, value)
		}
	}
	for k, v := range values {
		env = append(env, k+"="+v)
	}
	return env
}

func StartProcess(t testing.TB, binary, mode string, env map[string]string) *Process {
	t.Helper()
	p := &Process{cmd: exec.Command(binary, mode), done: make(chan struct{}), logPath: filepath.Join(t.TempDir(), "process.log")}
	log, err := os.Create(p.logPath)
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Env = Environment(env)
	p.cmd.Stdout, p.cmd.Stderr = log, log
	if err := p.cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	go func() { p.result = p.cmd.Wait(); log.Close(); close(p.done) }()
	t.Cleanup(func() {
		if err := p.Stop(8 * time.Second); err != nil {
			t.Errorf("process shutdown: %v\n%s", err, p.Log())
		}
		if t.Failed() {
			t.Log(p.Log())
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	for {
		f, err := os.Open(p.logPath)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var event struct {
				Listener        string `json:"listener"`
				Address         string `json:"address"`
				GatewayInstance string `json:"gateway_instance"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) == nil {
				if event.GatewayInstance != "" {
					p.GatewayInstance = event.GatewayInstance
				}
				if event.Listener == "public" {
					p.PublicOrigin = "http://" + event.Address
				}
				if event.Listener == "private" {
					p.PrivateOrigin = "http://" + event.Address
				}
			}
		}
		f.Close()
		if p.PrivateOrigin != "" && (mode == "worker" || p.PublicOrigin != "") {
			resp, err := client.Get(p.PrivateOrigin + "/health/ready")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return p
				}
			}
		}
		select {
		case <-p.done:
			t.Fatalf("process exited before readiness: %v\n%s", p.result, p.Log())
		case <-ctx.Done():
			t.Fatalf("process startup timed out\n%s", p.Log())
		case <-ticker.C:
		}
	}
}

func (p *Process) Log() string { data, _ := os.ReadFile(p.logPath); return string(data) }

// Kill terminates the process the way a lost machine does, without giving it
// the chance to drain, and waits for it to be reaped. A process stopped this
// way exited uncleanly on purpose, so Stop no longer reports its exit status as
// a shutdown failure.
func (p *Process) Kill() error {
	select {
	case <-p.done:
		return fmt.Errorf("process had already exited before it was killed: %v", p.result)
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return err
	}
	p.killed.Store(true)
	<-p.done
	return nil
}

func (p *Process) Stop(timeout time.Duration) error {
	if p.killed.Load() {
		<-p.done
		return nil
	}
	select {
	case <-p.done:
		return p.result
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.result
	case <-timer.C:
		p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("process exceeded shutdown deadline %s", timeout)
	}
}
