package codexwire

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

type Observation struct {
	Terminal   bool
	Usage      codemode.Usage
	ResponseID string
	Status     int
	Allowance  *codemode.Allowance
}

func Observe(body []byte, unary bool) Observation {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return Observation{}
	}
	root := document.Root()
	kind, _ := field(root, "type").Text()
	if kind == "codex.rate_limits" {
		primary := field(field(root, "rate_limits"), "primary")
		headers := http.Header{"X-Codex-Primary-Used-Percent": {field(primary, "used_percent").Raw()}, "X-Codex-Primary-Reset-At": {field(primary, "reset_at").Raw()}}
		return Observation{Allowance: Allowance(headers, time.Now().UTC())}
	}
	if kind == "error" {
		status, _ := strconv.Atoi(field(root, "status").Raw())
		if status == 0 {
			status, _ = strconv.Atoi(field(root, "status_code").Raw())
		}
		if status < 400 || status > 599 {
			status = 0
		}
		headers := http.Header{}
		for _, name := range []string{"x-codex-primary-used-percent", "x-codex-primary-reset-at"} {
			value := field(field(root, "headers"), name)
			if text, ok := value.Text(); ok {
				headers.Set(name, text)
			} else if value.Kind() == oif.Number {
				headers.Set(name, value.Raw())
			}
		}
		return Observation{Terminal: true, Status: status, Allowance: Allowance(headers, time.Now().UTC())}
	}
	terminal := kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed"
	if terminal {
		root = field(root, "response")
	}
	if unary {
		status, _ := field(root, "status").Text()
		terminal = status == "completed" || status == "incomplete" || status == "failed"
		object, _ := field(root, "object").Text()
		terminal = terminal || object == "response.compaction"
	}
	if !terminal {
		return Observation{}
	}
	id, _ := field(root, "id").Text()
	usage := field(root, "usage")
	read := func(value oif.Value) *int64 {
		if value.Kind() != oif.Number {
			return nil
		}
		n, err := strconv.ParseInt(value.Raw(), 10, 64)
		if err != nil || n < 0 || n > 1<<53-1 {
			return nil
		}
		return &n
	}
	u := codemode.Usage{Total: read(field(usage, "total_tokens")), Input: read(field(usage, "input_tokens")), Output: read(field(usage, "output_tokens")), Cached: read(field(field(usage, "input_tokens_details"), "cached_tokens")), Reasoning: read(field(field(usage, "output_tokens_details"), "reasoning_tokens"))}
	if u.Validate() != nil {
		u = codemode.Usage{}
	}
	return Observation{Terminal: true, Usage: u, ResponseID: id}
}

// Stream observes bounded SSE events independently of the forwarded byte stream.
type Stream struct {
	line           []byte
	data           []byte
	overflow       bool
	carriageReturn bool
	limit          int
	Emit           func(Observation)
}

func NewStream(limit int, emit func(Observation)) *Stream { return &Stream{limit: limit, Emit: emit} }

func (s *Stream) Write(data []byte) {
	for _, b := range data {
		if s.carriageReturn && b == '\n' {
			s.carriageReturn = false
			continue
		}
		s.carriageReturn = b == '\r'
		if b != '\n' && b != '\r' {
			if len(s.line) < s.limit {
				s.line = append(s.line, b)
			} else {
				s.overflow = true
			}
			continue
		}
		line := bytes.TrimSuffix(s.line, []byte{'\r'})
		if len(line) == 0 {
			if !s.overflow && len(s.data) > 0 {
				s.Emit(Observe(bytes.TrimSuffix(s.data, []byte{'\n'}), false))
			}
			s.data = s.data[:0]
			s.overflow = false
		} else if bytes.HasPrefix(line, []byte("data:")) && !s.overflow {
			value := bytes.TrimPrefix(line[5:], []byte{' '})
			if len(s.data)+len(value)+1 > s.limit {
				s.overflow = true
			} else {
				s.data = append(s.data, value...)
				s.data = append(s.data, '\n')
			}
		}
		s.line = s.line[:0]
	}
}

func Allowance(headers http.Header, now time.Time) *codemode.Allowance {
	values := headers.Values("x-codex-primary-used-percent")
	if len(values) != 1 {
		return nil
	}
	used, err := strconv.ParseFloat(values[0], 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return nil
	}
	remaining := 100 - used
	result := &codemode.Allowance{RemainingPercent: &remaining, ObservedAt: now}
	if values := headers.Values("x-codex-primary-reset-at"); len(values) == 1 {
		if seconds, err := strconv.ParseInt(values[0], 10, 64); err == nil && seconds > 0 && seconds < 253402300800 {
			reset := time.Unix(seconds, 0).UTC()
			result.ResetsAt = &reset
		}
	}
	return result
}

// ForwardHeaders excludes authentication, OLP controls and hop-by-hop fields.
func ForwardHeaders(source http.Header, request bool) http.Header {
	result := source.Clone()
	for _, connection := range source.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			result.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		result.Del(name)
	}
	for name := range result {
		if strings.HasPrefix(strings.ToLower(name), "x-olp-") {
			result.Del(name)
		}
	}
	if request {
		for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "Cookie", "X-Api-Key", "X-Goog-Api-Key"} {
			result.Del(name)
		}
	}
	return result
}
