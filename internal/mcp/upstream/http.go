package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/oif"
)

type httpConnection struct {
	client                   *http.Client
	endpoint, token, session string
	listChanged              atomic.Bool
}

func (c *httpConnection) changed() bool { return c.listChanged.Load() }
func (c *httpConnection) observeNotification(raw []byte) {
	note, code := messages.Parse(raw, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 64, MaxNodes: 1 << 16})
	if code == 0 && note.Kind == messages.Notification && note.Method == "notifications/tools/list_changed" {
		c.listChanged.Store(true)
	}
}

func (c *httpConnection) applyHeaders(h http.Header, protocol string) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		h.Set("Mcp-Session-Id", c.session)
	}
	if protocol != "" {
		h.Set("MCP-Protocol-Version", protocol)
	}
}

func (c *httpConnection) exchange(ctx context.Context, protocol string, id int, method string, params json.RawMessage) (json.RawMessage, error) {
	requestID := func() json.RawMessage {
		if id == 0 {
			return nil
		}
		return json.RawMessage(strconv.Itoa(id))
	}()
	body, err := json.Marshal(messages.Request{JSONRPC: "2.0", ID: requestID, Method: method, Params: params})
	if err != nil {
		return nil, ErrTransport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.applyHeaders(req.Header, protocol)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, ErrTransport
	}
	defer resp.Body.Close()
	if id == 0 {
		if resp.StatusCode != 202 && resp.StatusCode != 204 && resp.StatusCode != 200 {
			return nil, ErrTransport
		}
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, ErrTransport
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
		if len(c.session) > 256 || strings.ContainsAny(c.session, "\r\n\x00") {
			return nil, ErrTransport
		}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, ErrTransport
	}
	reader := &io.LimitedReader{R: resp.Body, N: (1 << 20) + 1}
	if media == "application/json" {
		raw, err := io.ReadAll(reader)
		if err != nil || len(raw) > 1<<20 {
			return nil, ErrTransport
		}
		return raw, nil
	}
	if media != "text/event-stream" {
		return nil, ErrTransport
	}
	eventLimit := sseEventLimit(method)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), eventLimit)
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) != 0 {
				if _, matchErr := matchingResult(data, id); matchErr == nil {
					return data, nil
				}
				c.observeNotification(data)
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
		if len(data) > eventLimit || reader.N == 0 {
			return nil, ErrTransport
		}
	}
	return nil, ErrTransport
}
func sseEventLimit(method string) int {
	if method == "tools/call" {
		return 1 << 20
	}
	return 128 << 10
}

func (c *httpConnection) close(ctx context.Context, protocol string) {
	if c.client != nil {
		defer c.client.CloseIdleConnections()
	}
	if c.session == "" {
		return
	}
	closeContext, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	r, err := http.NewRequestWithContext(closeContext, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	c.applyHeaders(r.Header, protocol)
	if response, err := c.client.Do(r); err == nil {
		response.Body.Close()
	}
}
