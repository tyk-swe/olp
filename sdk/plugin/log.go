package plugin

import (
	"context"
	"log/slog"
	"maps"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Log writes to OLP's log. OLP redacts the secret values of the current call
// from every record and bounds how much one call may log, so a plugin may log
// freely without leaking what OLP gave it. Anything the module writes to
// standard output or standard error is logged the same way.
var Log = slog.New(hostHandler{})

// hostHandler sends records to OLP's log capability. Attribute values travel
// as text, and groups flatten into dotted keys.
type hostHandler struct {
	attrs  map[string]string
	prefix string
}

func (hostHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h hostHandler) Handle(_ context.Context, r slog.Record) error {
	_, err := callHost(abi.CapabilityLog, h.record(r))
	return err
}

func (h hostHandler) record(r slog.Record) abi.LogRecord {
	record := abi.LogRecord{Level: levelName(r.Level), Message: r.Message, Attrs: maps.Clone(h.attrs)}
	if record.Attrs == nil {
		record.Attrs = map[string]string{}
	}
	r.Attrs(func(a slog.Attr) bool {
		flatten(record.Attrs, h.prefix, a)
		return true
	})
	return record
}

func (h hostHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := hostHandler{attrs: maps.Clone(h.attrs), prefix: h.prefix}
	if next.attrs == nil {
		next.attrs = map[string]string{}
	}
	for _, a := range attrs {
		flatten(next.attrs, h.prefix, a)
	}
	return next
}

func (h hostHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return hostHandler{attrs: h.attrs, prefix: h.prefix + name + "."}
}

func flatten(into map[string]string, prefix string, a slog.Attr) {
	value := a.Value.Resolve()
	if value.Kind() != slog.KindGroup {
		if a.Key != "" {
			into[prefix+a.Key] = value.String()
		}
		return
	}
	if a.Key != "" {
		prefix += a.Key + "."
	}
	for _, member := range value.Group() {
		flatten(into, prefix, member)
	}
}

func levelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	}
	return "error"
}
