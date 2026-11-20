package coordination

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"strconv"
	"strings"
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

func TestCommandDeadlineUsesConfiguredTimeout(t *testing.T) {
	t.Parallel()
	const timeout = 30 * time.Second
	configuration, err := Configuration("redis://localhost:6379", "", timeout)
	if err != nil {
		t.Fatal(err)
	}
	deadlines := make(chan time.Time, 32)
	configuration.option.DialCtxFn = func(context.Context, string, *net.Dialer, *tls.Config) (net.Conn, error) {
		client, server := net.Pipe()
		t.Cleanup(func() { client.Close(); server.Close() })
		go answerValkey(server, "")
		return deadlineConnection{Conn: client, deadlines: deadlines}, nil
	}
	client, err := Open(t.Context(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for len(deadlines) > 0 {
		<-deadlines
	}
	started := time.Now()
	if err := client.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case deadline := <-deadlines:
		if remaining := deadline.Sub(started); remaining < timeout-time.Second || remaining > timeout+time.Second {
			t.Fatalf("command transport deadline is %s after start, want configured %s", remaining, timeout)
		}
	default:
		t.Fatal("command did not set a transport deadline")
	}
}

type deadlineConnection struct {
	net.Conn
	deadlines chan<- time.Time
}

func (c deadlineConnection) SetDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		c.deadlines <- deadline
	}
	return c.Conn.SetDeadline(deadline)
}

func TestSearchRejectsPartialResultsOnTimeout(t *testing.T) {
	t.Parallel()
	const result = "+total_results\r\n:1\r\n+results\r\n*1\r\n%2\r\n+id\r\n+entry\r\n+extra_attributes\r\n%1\r\n+distance\r\n+0\r\n"
	for name, tc := range map[string]struct {
		reply string
		fails bool
	}{
		"RESP2":   {reply: "*3\r\n:1\r\n+entry\r\n*2\r\n+distance\r\n+0\r\n"},
		"RESP3":   {reply: "%3\r\n" + result + "+error\r\n*0\r\n"},
		"timeout": {reply: "%3\r\n" + result + "+error\r\n*1\r\n-Timeout limit was reached\r\n", fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := Configuration("redis://localhost:6379", "", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			config.option.DialCtxFn = func(context.Context, string, *net.Dialer, *tls.Config) (net.Conn, error) {
				client, server := net.Pipe()
				go answerValkey(server, tc.reply)
				return client, nil
			}
			client, err := Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			docs, err := client.Search(t.Context(), "fixtures", "*", "RETURN", "1", "distance")
			if tc.fails {
				if err == nil || len(docs) != 0 {
					t.Fatalf("timed-out search returned %v, %v", docs, err)
				}
			} else if err != nil || len(docs) != 1 || docs[0].Key != "entry" || docs[0].Doc["distance"] != "0" {
				t.Fatalf("successful search returned %v, %v", docs, err)
			}
		})
	}
}

func answerValkey(conn net.Conn, searchReply string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
		if err != nil {
			return
		}
		command := ""
		for index := range count {
			lengthLine, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			length, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lengthLine, "$")))
			if err != nil || length < 0 {
				return
			}
			part := make([]byte, length+2)
			if _, err := io.ReadFull(reader, part); err != nil {
				return
			}
			if index == 0 {
				command = string(part[:length])
			}
		}
		response := "+OK\r\n"
		if command == "HELLO" {
			response = "%1\r\n+proto\r\n:3\r\n"
		}
		if command == "PING" {
			response = "+PONG\r\n"
		}
		if command == "FT.SEARCH" {
			response = searchReply
		}
		if _, err := io.WriteString(conn, response); err != nil {
			return
		}
	}
}
