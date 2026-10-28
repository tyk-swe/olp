package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type input struct {
	Query            string `json:"query"`
	Command          string `json:"command,omitempty"`
	Path             string `json:"path,omitempty"`
	Env              string `json:"env,omitempty"`
	Cwd              string `json:"cwd,omitempty"`
	Environment      string `json:"environment,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

func main() {
	if len(os.Args) != 1 || len(os.Environ()) != 0 {
		os.Exit(2)
	}
	if err := appendRecord("starts", strconv.Itoa(os.Getpid())+"\n"); err != nil {
		os.Exit(3)
	}
	toolName := fixtureToolName()
	finalReply := &atomic.Bool{}
	server := mcp.NewServer(&mcp.Implementation{Name: "installed-tools", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	mcp.AddTool(server, &mcp.Tool{Name: toolName}, func(_ context.Context, _ *mcp.CallToolRequest, arguments input) (*mcp.CallToolResult, *struct{}, error) {
		if err := appendRecord("effects", "called\n"); err != nil {
			return nil, nil, err
		}
		if arguments.Query == "exit" {
			os.Exit(7)
		}
		if arguments.Query == "wait" {
			select {}
		}
		if arguments.Query == "oversize" {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 2<<20)}}}, nil, nil
		}
		if arguments.Query == "final" {
			finalReply.Store(true)
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.IOTransport{Reader: os.Stdin, Writer: finalOutput{File: os.Stdout, exit: finalReply}, MaxLineLength: 1 << 20}); err != nil {
		os.Exit(1)
	}
}

func appendRecord(path, text string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, writeErr := file.WriteString(text)
	return writeErr
}

func fixtureToolName() string {
	if _, err := os.Stat("drift"); err == nil {
		return "echo_changed"
	}
	return "echo"
}

type finalOutput struct {
	*os.File
	exit *atomic.Bool
}

func (output finalOutput) Write(data []byte) (int, error) {
	written, writeErr := output.File.Write(data)
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	if writeErr == nil && output.exit.Load() && json.Unmarshal(data, &response) == nil && len(response.Result) > 0 {
		os.Exit(0)
	}
	return written, writeErr
}
