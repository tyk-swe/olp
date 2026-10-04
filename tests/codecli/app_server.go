package codecli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type rpcMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// ReviewAfterTask drives the official app-server protocol without a browser.
// The first turn is an explicit user task, never a setup probe to acquire a pin.
func (c *Client) ReviewAfterTask(t testing.TB, flags ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := c.Command(ctx, append([]string{"app-server"}, flags...)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
	read := func() rpcMessage {
		var message rpcMessage
		if err := decoder.Decode(&message); err != nil {
			t.Fatalf("official app-server response: %v", err)
		}
		return message
	}
	sequence := 0
	var notifications []rpcMessage
	call := func(method, params string) json.RawMessage {
		sequence++
		request := struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}{sequence, method, json.RawMessage(params)}
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		for {
			message := read()
			if message.ID != sequence {
				notifications = append(notifications, message)
				continue
			}
			if len(message.Error) != 0 {
				t.Fatalf("%s: %s", method, message.Error)
			}
			return message.Result
		}
	}
	call("initialize", `{"clientInfo":{"name":"olp_qualification","version":"1"}}`)
	if _, err := io.WriteString(stdin, "{\"method\":\"initialized\"}\n"); err != nil {
		t.Fatal(err)
	}
	result := call("thread/start", `{"model":"gpt-5.4","approvalPolicy":"never","sandbox":"read-only"}`)
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("thread/start: %s (%v)", result, err)
	}
	id, _ := json.Marshal(started.Thread.ID)
	wait := func() {
		for {
			var message rpcMessage
			if len(notifications) > 0 {
				message, notifications = notifications[0], notifications[1:]
			} else {
				message = read()
			}
			if message.Method != "turn/completed" {
				continue
			}
			var completed struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					Status string          `json:"status"`
					Error  json.RawMessage `json:"error"`
				} `json:"turn"`
			}
			if err := json.Unmarshal(message.Params, &completed); err != nil {
				t.Fatal(err)
			}
			if completed.ThreadID != started.Thread.ID {
				continue
			}
			if completed.Turn.Status != "completed" {
				t.Fatalf("official turn failed: %s", message.Params)
			}
			return
		}
	}
	call("turn/start", `{"threadId":`+string(id)+`,"input":[{"type":"text","text":"Complete this explicit controlled coding task."}]}`)
	wait()
	call("review/start", `{"threadId":`+string(id)+`,"delivery":"inline","target":{"type":"custom","instructions":"Review the completed controlled coding task."}}`)
	wait()
	if _, err := os.Stat(filepath.Join(c.Home, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("app-server created login material: %v", err)
	}
	return started.Thread.ID
}
