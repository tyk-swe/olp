package connectors

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

const watsonxVersion = "2025-10-25"

var watsonxModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*$`)

func watsonxModelValid(model string) bool {
	return len(model) <= 256 && watsonxModel.MatchString(model) && !strings.Contains(model, "..")
}

func (c Config) watsonxURL(base string, encodedWire wire.Format, stream bool) (string, error) {
	if encodedWire != wire.Chat {
		return "", errors.New("watsonx serves its chat API only")
	}
	path := "/ml/v1/text/chat"
	if stream {
		path += "_stream"
	}
	return base + path + "?version=" + url.QueryEscape(WatsonxVersion(c.APIVersion)), nil
}

func WatsonxVersion(configured string) string {
	if configured == "" {
		return watsonxVersion
	}
	return configured
}

func (c Config) watsonxBody(body []byte) []byte {
	return rewriteObject(body, func(fields map[string]json.RawMessage) {
		if model, named := fields["model"]; named {
			fields["model_id"] = model
			delete(fields, "model")
		}
		fields["project_id"], _ = json.Marshal(c.CloudProject)
		// The address selects streaming, and every stream reports its usage.
		delete(fields, "stream")
		delete(fields, "stream_options")
		// watsonx keeps tool_choice for a named function and spells the modes
		// none, auto and required as tool_choice_option.
		if choice := fields["tool_choice"]; len(choice) > 0 && choice[0] == '"' {
			fields["tool_choice_option"] = choice
			delete(fields, "tool_choice")
		}
	})
}

func watsonxResult(body []byte, object string) (result []byte, usage bool) {
	result = rewriteObject(body, func(fields map[string]json.RawMessage) {
		if model, named := fields["model_id"]; named {
			if _, present := fields["model"]; !present {
				fields["model"] = model
			}
			delete(fields, "model_id")
		}
		delete(fields, "created_at")
		delete(fields, "system")
		if _, typed := fields["object"]; !typed {
			fields["object"] = json.RawMessage(strconv.Quote(object))
		}
		usage = len(fields["usage"]) > 0 && string(fields["usage"]) != "null"
	})
	return result, usage
}

type watsonxStream struct {
	events   *sse.Decoder
	pending  []byte
	terminal bool
	done     bool
}

func (s *watsonxStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if s.done {
			return 0, io.EOF
		}
		frame, err := s.events.Next()
		switch {
		case err == io.EOF:
			s.done = true
			if s.terminal {
				s.pending = []byte("data: [DONE]\n\n")
			}
			continue
		case err != nil:
			return 0, err
		case frame.Event != nil && *frame.Event == "error":
			if stated := openai.ParseErrorBody([]byte(frame.Data)); stated != nil {
				return 0, stated
			}
			return 0, &wire.UpstreamError{Message: "watsonx stream failed"}
		case frame.Data == "[DONE]":
			s.done = true
			s.pending = []byte("data: [DONE]\n\n")
			continue
		}
		chunk, usage := watsonxResult([]byte(frame.Data), "chat.completion.chunk")
		s.terminal = s.terminal || usage
		s.pending = sse.Frame{Data: string(chunk)}.Encode()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}
