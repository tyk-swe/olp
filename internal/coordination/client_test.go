package coordination

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestOpenStopsAtCallerDeadline(t *testing.T) {
	t.Parallel()
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		if conn, err := silent.Accept(); err == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		silent.Close()
		select {
		case conn := <-accepted:
			conn.Close()
		default:
		}
	})
	configuration, err := Configuration("redis://"+silent.Addr().String(), "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	client, err := Open(ctx, configuration)
	if err == nil {
		client.Close()
		t.Fatal("Open succeeded against a server that never answers")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Open took %s, past the caller's 50ms deadline", elapsed)
	}
}

func TestConfigurationDatabasePaths(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]int{"": 0, "/": 0, "/0": 0, "/3": 3} {
		t.Run("accepted"+path, func(t *testing.T) {
			if config, err := Configuration("redis://localhost:6379"+path, "", time.Second); err != nil || config.option.SelectDB != want {
				t.Fatalf("Configuration(%q): database %d, error %v, want %d", path, config.option.SelectDB, err, want)
			}
		})
	}
	for _, path := range []string{"/abc", "/-1", "/3/", "//"} {
		t.Run("rejected"+path, func(t *testing.T) {
			if _, err := Configuration("redis://localhost:6379"+path, "", time.Second); err == nil {
				t.Fatalf("Configuration(%q) accepted an invalid database", path)
			}
		})
	}
}
