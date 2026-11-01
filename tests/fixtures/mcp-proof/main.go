package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/testutil"
)

type echoInput struct {
	Message string `json:"message" jsonschema:"Text to return"`
}

type echoOutput struct {
	Message string `json:"message"`
}

type endpoints struct {
	MCP           string `json:"mcp"`
	Authorization string `json:"authorization"`
	Token         string `json:"token"`
	Device        string `json:"device"`
}

func echo(_ context.Context, _ *mcp.CallToolRequest, input echoInput) (*mcp.CallToolResult, echoOutput, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: input.Message}}}, echoOutput{Message: input.Message}, nil
}

func serve(ctx context.Context, address string) error {
	server := mcp.NewServer(&mcp.Implementation{Name: "tool-proof-fixture", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Return a fixture message"}, echo)
	if address == "" {
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	authority := testutil.StartOAuthServer()
	defer authority.Close()
	authority.SignInAs(testutil.OAuthIdentity{Subject: "fixture-user", Account: "fixture-account"})
	transport := mcp.NewStreamableHTTPHandler(func(_ *http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		_, authorized := authority.Authorized(r)
		if r.Header.Get("Authorization") != "" && !authorized {
			http.Error(w, "Unknown fixture credential", http.StatusUnauthorized)
			return
		}
		transport.ServeHTTP(w, r)
	})
	listener, listenErr := net.Listen("tcp", address)
	if listenErr != nil {
		return listenErr
	}
	defer listener.Close()
	if encodeErr := json.NewEncoder(os.Stdout).Encode(endpoints{
		MCP:           "http://" + listener.Addr().String() + "/mcp",
		Authorization: authority.URL + "/authorize",
		Token:         authority.URL + "/token",
		Device:        authority.URL + "/device/code",
	}); encodeErr != nil {
		return encodeErr
	}
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	defer httpServer.Close()
	shutdown, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		<-shutdown.Done()
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(deadline)
	}()
	return httpServer.Serve(listener)
}

func main() {
	address := flag.String("http", "", "Serve HTTP and OAuth fixtures instead of stdio")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *address); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
