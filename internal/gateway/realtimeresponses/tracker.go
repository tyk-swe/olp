package realtimeresponses

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

// The wire relay does not buffer native frames. This small state machine only
// records whether a response is still owed when either peer closes normally.
// A provider-controlled ID set is bounded; an untrackable event conservatively
// makes a clean socket close incomplete instead of claiming terminal delivery.
const maxPending = 128
const maxFrameBytes = 1 << 20

type Tracker struct {
	requested int
	active    map[string]struct{}
	anonymous bool
	unknown   bool
}

func (s *Tracker) Pending() bool {
	return s.requested > 0 || len(s.active) > 0 || s.anonymous || s.unknown
}

func (s *Tracker) Untrackable() { s.unknown = true }

func (s *Tracker) add(id string, strict bool) {
	if id == "" || len(id) > 512 {
		s.unknown = true
		return
	}
	if s.active == nil {
		s.active = make(map[string]struct{})
	}
	if _, found := s.active[id]; found {
		return
	}
	if len(s.active) >= maxPending {
		s.unknown = true
		return
	}
	if strict {
		// A strict decoder may return a view into the source frame. Keep
		// only the bounded identifier, never the audio-bearing frame.
		id = strings.Clone(id)
	}
	s.active[id] = struct{}{}
}

func (s *Tracker) ClientFrame(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText || len(data) > maxFrameBytes {
		s.unknown = true
		return
	}
	eventType := ""
	if flat, ok := flatFrame(data); ok {
		eventType = flat.Type
	} else {
		root, valid := parseObject(data)
		if !valid {
			s.unknown = true
			return
		}
		eventType, valid = stringField(root, "type")
		if !valid {
			s.unknown = true
			return
		}
	}
	// Only response.create asks for a model response. Committing the input
	// audio buffer never creates one, so a manual turn sends both and the
	// provider answers once.
	if eventType == "response.create" {
		if s.requested == maxPending {
			s.unknown = true
		} else {
			s.requested++
		}
	}
}

func (s *Tracker) ProviderFrame(typ websocket.MessageType, data []byte, strict bool) *wire.Usage {
	if typ != websocket.MessageText || len(data) > maxFrameBytes {
		s.unknown = true
		return nil
	}
	if !strict && !bytes.Contains(data, []byte("response.done")) && bytes.IndexByte(data, '\\') < 0 {
		// Transformed sessions do not promise terminal response tracking. They
		// observe only response.done usage, and that exact type must appear
		// literally or contain a JSON escape. Other native frames still pass
		// through byte-for-byte without decoding or changed failure behavior.
		return nil
	}
	var event frame
	valid := false
	if strict {
		valid = decodeStrict(data, &event)
	} else {
		valid = json.Unmarshal(data, &event) == nil
	}
	if !valid {
		s.unknown = true
		return nil
	}
	id := event.ResponseID
	if event.Response != nil && event.Response.ID != "" {
		id = event.Response.ID
	}
	switch event.Type {
	case "response.created":
		if s.requested > 0 {
			s.requested--
		}
		s.add(id, strict)
	case "response.done":
		if s.anonymous && len(s.active) > 0 {
			// Without a response ID on a preceding fragment, this terminal
			// cannot prove which concurrent response that fragment belonged to.
			s.unknown = true
		}
		if id != "" {
			if _, found := s.active[id]; found {
				delete(s.active, id)
			} else if s.requested > 0 {
				s.requested--
			} else if len(s.active) > 0 {
				s.unknown = true
			}
		} else if len(s.active) > 0 {
			s.unknown = true
		} else if s.requested > 0 {
			s.requested--
		}
		// Some native response fragments do not identify their response.
		// A terminal response closes that observation, while an unparseable or
		// unmatched event remains uncertain for the rest of the session.
		s.anonymous = false
	default:
		if strings.HasPrefix(event.Type, "response.") {
			if id != "" {
				if _, found := s.active[id]; !found && s.requested > 0 {
					s.requested--
				}
				s.add(id, strict)
			} else {
				s.anonymous = true
			}
		}
	}
	if event.Type != "response.done" {
		return nil
	}
	if event.Response == nil || event.Response.Usage == nil {
		s.unknown = true
		return nil
	}
	u := event.Response.Usage
	if u.InputTokens == nil || u.OutputTokens == nil || *u.InputTokens < 0 || *u.OutputTokens < 0 {
		s.unknown = true
		return nil
	}
	out := &wire.Usage{InputTokens: *u.InputTokens, OutputTokens: *u.OutputTokens}
	if u.InputDetails != nil {
		if u.InputDetails.CachedTokens != nil {
			cached := *u.InputDetails.CachedTokens
			if cached < 0 || cached > *u.InputTokens {
				s.unknown = true
				return nil
			}
			out.CachedInputTokens = &cached
		}
		out.AudioInputTokens = u.InputDetails.AudioTokens
		out.ImageInputTokens = u.InputDetails.ImageTokens
		if u.InputDetails.CachedDetails != nil {
			out.CachedAudioInputTokens = u.InputDetails.CachedDetails.AudioTokens
			out.CachedImageInputTokens = u.InputDetails.CachedDetails.ImageTokens
		}
	}
	if u.OutputDetails != nil {
		out.AudioOutputTokens = u.OutputDetails.AudioTokens
	}
	return out
}
