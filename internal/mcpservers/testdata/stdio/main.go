package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type input struct {
	Query            string `json:"query"`
	Command          string `json:"command,omitempty"`
	Path             string `json:"path,omitempty"`
	Environment      string `json:"environment,omitempty"`
	WorkingDirectory string `json:"working_directory,omitempty"`
}

func main() {
	if len(os.Args) != 1 || len(os.Environ()) != 0 {
		os.Exit(2)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "installed-tools", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, arguments input) (*mcp.CallToolResult, any, error) {
		file, err := os.OpenFile("effects", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, nil, err
		}
		_, writeErr := file.WriteString("called\n")
		closeErr := file.Close()
		if writeErr != nil {
			return nil, nil, writeErr
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		if arguments.Query == "exit" {
			os.Exit(7)
		}
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
	})
	if err := server.Run(context.Background(), &mcp.IOTransport{Reader: os.Stdin, Writer: os.Stdout, MaxLineLength: 1 << 20}); err != nil {
		os.Exit(1)
	}
}
