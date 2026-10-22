package gateway

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/coderesearch"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

type researchResponse struct {
	method, service, resumeKey string
	requestID                  json.RawMessage
	answerPing                 func([]byte) error
}

func researchOriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	u, err := url.Parse(values[0])
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

// copyResearchResponse reports whether a valid InitializeResult reached the
// caller. Tentative sessions must be removed when it returns false.
func (s *Server) copyResearchResponse(w http.ResponseWriter, r *http.Request, response *http.Response, forwarding researchResponse) bool {
	refuse := func() { codeWriteError(w, r, codemode.Refuse(502, "code_research_response_invalid")) }
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		refuse()
		return false
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	for _, name := range []string{"Retry-After", "Mcp-Protocol-Version"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if media != "text/event-stream" || response.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(io.LimitReader(response.Body, coderesearch.MaxResponse+1))
		if err != nil || len(raw) > coderesearch.MaxResponse {
			refuse()
			return false
		}
		if response.StatusCode == http.StatusOK && len(raw) != 0 {
			if media != "application/json" {
				refuse()
				return false
			}
			raw, err = coderesearch.Response(raw, forwarding.method, forwarding.service)
			if err != nil {
				refuse()
				return false
			}
		}
		if media != "" {
			w.Header().Set("Content-Type", media)
		}
		w.WriteHeader(response.StatusCode)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		n, err := w.Write(raw)
		return err == nil && n == len(raw) && response.StatusCode == http.StatusOK && forwarding.method == "initialize" && coderesearch.Initialized(raw, forwarding.requestID)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	reader := &io.LimitedReader{R: response.Body, N: coderesearch.MaxResponse + 1}
	decoder := sse.NewDecoder(reader, coderesearch.MaxEvent)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if err := controller.Flush(); err != nil {
		return false
	}
	_ = controller.SetWriteDeadline(time.Time{})
	session := r.Header.Get("Mcp-Session-Id")
	if session == "" {
		session = w.Header().Get("Mcp-Session-Id")
	}
	initialized, pings := false, 0
	for {
		frame, err := decoder.NextFrame()
		if err != nil || reader.N <= 0 {
			return initialized
		}
		if !frame.Control && strings.TrimSpace(frame.Data) != "" {
			reply, err := coderesearch.PingResponse([]byte(frame.Data))
			if err != nil {
				return initialized
			}
			if reply != nil {
				// Pings are control traffic within the admitted stream. Bound
				// their count as well as their individual response deadline.
				pings++
				if pings > 32 || forwarding.answerPing == nil || forwarding.answerPing(reply) != nil {
					return initialized
				}
				continue
			}
			filtered, err := coderesearch.Response([]byte(frame.Data), forwarding.method, forwarding.service)
			if err != nil {
				return initialized
			}
			if filtered == nil {
				continue
			}
			frame.Data = string(filtered)
		}
		if frame.ID != nil && *frame.ID != "" {
			sealed, err := coderesearch.SealEventID(session, *frame.ID, forwarding.resumeKey)
			if err != nil {
				return initialized
			}
			frame.ID = &sealed
		}
		_ = controller.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		encoded := frame.Encode()
		if n, err := w.Write(encoded); err != nil || n != len(encoded) {
			return initialized
		}
		if err = controller.Flush(); err != nil {
			return initialized
		}
		_ = controller.SetWriteDeadline(time.Time{})
		if forwarding.method == "initialize" && !frame.Control && coderesearch.Initialized([]byte(frame.Data), forwarding.requestID) {
			initialized = true
		}
	}
}
