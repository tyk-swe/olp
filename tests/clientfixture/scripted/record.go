package scripted

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Record is one request the upstream received, in arrival order. Rejected
// requests are recorded too, so a client that never reaches the upstream and
// a gateway that sends it something invalid are both visible.
type Record struct {
	Seq     int    `json:"seq"`
	Dialect string `json:"dialect"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Query   string `json:"query,omitempty"`
	// Headers are lower-cased. Credential headers keep only their presence.
	Headers map[string]string `json:"headers"`
	// Model is the upstream model the request addressed, from the body or the
	// path.
	Model string `json:"model,omitempty"`
	// Body is the JSON body; BodyText carries any other body.
	Body     json.RawMessage `json:"body,omitempty"`
	BodyText string          `json:"body_text,omitempty"`
	// Authorized reports whether the upstream credential was valid.
	Authorized bool `json:"authorized"`
	// Status is the response status; Complete turns true when the handler
	// finished, so a reader can wait out a stream still in flight.
	Status   int  `json:"status"`
	Complete bool `json:"complete"`
	// Script names the scripted behavior that answered, for example
	// "tool_call" or "tool_result".
	Script string `json:"script,omitempty"`
	Stream bool   `json:"stream"`
	// LeakedClientCredential is true when a caller credential from
	// Options.ClientSecrets appeared anywhere in the request.
	LeakedClientCredential bool `json:"leaked_client_credential"`
}

// credentialHeaders carry secrets; the recording keeps only that they were sent.
var credentialHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"x-goog-api-key":      true,
	"cookie":              true,
}

func (f *Fixture) begin(dialect string, r *http.Request, body []byte) *Record {
	rec := &Record{
		Dialect: dialect, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
		Headers: map[string]string{},
	}
	scan := []string{r.URL.RequestURI(), string(body)}
	for name, values := range r.Header {
		name = strings.ToLower(name)
		joined := strings.Join(values, ", ")
		scan = append(scan, joined)
		if credentialHeaders[name] {
			joined = "[present]"
		}
		rec.Headers[name] = joined
	}
	for _, secret := range f.opts.ClientSecrets {
		for _, text := range scan {
			if secret != "" && strings.Contains(text, secret) {
				rec.LeakedClientCredential = true
			}
		}
	}
	// Gemini names the model in the path, the others in the body.
	if _, model, ok := strings.Cut(r.URL.Path, "/models/"); ok {
		rec.Model, _, _ = strings.Cut(model, ":")
	}
	if len(body) > 0 {
		if json.Valid(body) {
			rec.Body = json.RawMessage(bytes.TrimSpace(body))
			var probe struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(body, &probe) == nil && rec.Model == "" {
				rec.Model = strings.TrimPrefix(probe.Model, "models/")
			}
		} else if utf8.Valid(body) {
			rec.BodyText = string(body)
		} else {
			rec.BodyText = "[binary]"
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rec.Seq = f.seq
	f.records = append(f.records, rec)
	return rec
}

func (f *Fixture) finish(rec *Record, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	rec.Status, rec.Complete = status, true
}

// recorded returns a copy of the recording that matches the filters.
func (f *Fixture) recorded(q url.Values) ([]Record, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, inFlight := []Record{}, 0
	for _, rec := range f.records {
		if !rec.Complete {
			inFlight++
		}
		if match(rec, q) {
			out = append(out, *rec)
		}
	}
	return out, inFlight
}

// match applies the listing filters: dialect and path are prefixes, model is
// exact, and script is exact.
func match(rec *Record, q url.Values) bool {
	if v := q.Get("dialect"); v != "" && !strings.HasPrefix(rec.Dialect, v) {
		return false
	}
	if v := q.Get("path"); v != "" && !strings.HasPrefix(rec.Path, v) {
		return false
	}
	if v := q.Get("model"); v != "" && rec.Model != v {
		return false
	}
	if v := q.Get("script"); v != "" && rec.Script != v {
		return false
	}
	return true
}

func (f *Fixture) listRecorded(w http.ResponseWriter, r *http.Request) {
	requests, inFlight := f.recorded(r.URL.Query())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests, "in_flight": inFlight})
}

// resetRecorded forgets the recording and every piece of fixture state.
func (f *Fixture) resetRecorded(w http.ResponseWriter, r *http.Request) {
	f.reset()
	w.WriteHeader(http.StatusNoContent)
}
