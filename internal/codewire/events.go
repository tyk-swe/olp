package codewire

import "bytes"

// Events splits bounded SSE events independently of the forwarded byte
// stream. Emit receives each event's data lines joined by newlines; the slice
// is reused once Emit returns.
type Events struct {
	line           []byte
	data           []byte
	overflow       bool
	carriageReturn bool
	limit          int
	Emit           func([]byte)
}

func NewEvents(limit int, emit func([]byte)) *Events { return &Events{limit: limit, Emit: emit} }

func (s *Events) Write(data []byte) {
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
				s.Emit(bytes.TrimSuffix(s.data, []byte{'\n'}))
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
