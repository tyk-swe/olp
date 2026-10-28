package operationplan

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const (
	maxWireQueryParams = 32
	maxWireQueryValue  = 2048
	maxWireURL         = 8192
	pollLocationHeader = "Operation-Location"
)

func validateWireQuery(query url.Values) error {
	if len(query) > maxWireQueryParams {
		return fail("fidelity_protocol_violation", "/transport", "bounded_query", "The wire query is too large.")
	}
	for name, values := range query {
		if name == "" || len(name) > 128 || len(values) == 0 || len(values) > 8 {
			return fail("fidelity_protocol_violation", "/transport", "bounded_query", "The wire query is too large.")
		}
		for _, value := range values {
			if len(value) > maxWireQueryValue || strings.ContainsAny(value, "\r\n\x00") {
				return fail("fidelity_protocol_violation", "/transport", "bounded_query", "The wire query is too large.")
			}
		}
	}
	if len(query.Encode()) > maxWireURL {
		return fail("fidelity_protocol_violation", "/transport", "bounded_query", "The wire query is too large.")
	}
	return nil
}

func (p *Plan) PollLocation(submit string, header http.Header) (string, error) {
	raw := header.Get(pollLocationHeader)
	if raw == "" || len(raw) > maxWireURL {
		return "", fail("fidelity_protocol_violation", "/transport", "poll_location", "The polling location is missing or too large.")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", fail("fidelity_protocol_violation", "/transport", "poll_location", "The polling location is invalid.")
	}
	base, err := url.Parse(submit)
	if err != nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
		return "", fail("fidelity_protocol_violation", "/transport", "poll_location_origin", "The polling location leaves the bound provider origin.")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fail("fidelity_protocol_violation", "/transport", "poll_location", "The polling location is invalid.")
	}
	return raw, nil
}

func (p *Plan) PollState(body []byte) (operations.PollState, error) {
	if p.template.codec.Poll == nil {
		return "", fail("fidelity_protocol_violation", "/transport", "poll_reading", "The dialect has no polling reading.")
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: p.template.config.MaxBodyBytes})
	if err != nil {
		return "", operations.Violation("/result", "Native result is malformed or exceeds its declared bounds.")
	}
	return p.template.codec.Poll(doc)
}

func (p *Plan) CancelPoll(pollURL string) (string, string, bool) {
	if p.template.codec.CancelPoll == nil {
		return "", "", false
	}
	return p.template.codec.CancelPoll(pollURL)
}
